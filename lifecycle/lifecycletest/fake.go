// Package lifecycletest is an in-memory fake of the DynamoDB operations that
// package lifecycle uses, for unit tests that must not reach AWS.
//
// A Fake stands for DynamoDB in one region. It evaluates condition and update
// expressions with DynamoDB's semantics (see the subset documented in expr.go),
// applies TransactWriteItems atomically with per-item cancellation reasons,
// models a table's CREATING and DELETING states, and returns the SDK's own
// error types (ResourceNotFoundException, ConditionalCheckFailedException,
// TransactionCanceledException, ...), so the code under test exercises the same
// error handling it runs against AWS. Expressions outside the supported subset
// are rejected rather than guessed at.
//
// It is safe for concurrent use; every operation is atomic. Hook, if set, runs
// at the start of every operation, outside the lock, which is how a test drives
// a deterministic interleaving of several goroutines.
//
//	f := lifecycletest.New()
//	c, _ := lifecycle.New(lifecycle.Options{
//		DynamoDB: func(region string) (lifecycle.API, error) { return f, nil },
//	})
package lifecycletest

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"
)

// Fake is an in-memory DynamoDB for one region.
type Fake struct {
	// Hook, if set, is called at the start of every operation with the
	// operation's name ("GetItem", "TransactWriteItems", ...), before the
	// operation takes effect.
	Hook func(ctx context.Context, op string)

	// Err, if set, is consulted at the start of every operation, after Hook.
	// A non-nil result is returned as the operation's error and the operation
	// does nothing. Use it to inject throttling or network failures.
	Err func(op string, input any) error

	// CreateDelay is how many DescribeTable calls a new table answers with
	// CREATING before it becomes ACTIVE. DeleteDelay is how many answer
	// DELETING after DeleteTable before the table is gone. Zero means the
	// transition is immediate.
	CreateDelay, DeleteDelay int

	// ScanPage, if positive, caps the items one Scan returns when the request
	// sets no Limit, so that callers' pagination is exercised.
	ScanPage int

	mu        sync.Mutex
	tables    map[string]*table
	deletions []Deletion
	creates   []dynamodb.CreateTableInput
	ops       map[string]int
}

// Deletion records one DeleteTable call that succeeded.
type Deletion struct {
	Table string
	// Keys are the hash keys of the items the table held when it was deleted,
	// sorted.
	Keys []string
}

type table struct {
	key       string // hash key attribute name
	status    types.TableStatus
	countdown int
	items     map[string]map[string]types.AttributeValue
}

// New returns an empty Fake.
func New() *Fake {
	return &Fake{tables: map[string]*table{}, ops: map[string]int{}}
}

func (f *Fake) begin(ctx context.Context, op string, in any) error {
	if f.Hook != nil {
		f.Hook(ctx, op)
	}
	f.mu.Lock()
	f.ops[op]++
	f.mu.Unlock()
	if f.Err != nil {
		if err := f.Err(op, in); err != nil {
			return err
		}
	}
	return nil
}

// Calls returns how many times op was called (including calls that failed).
func (f *Fake) Calls(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ops[op]
}

// Deletions returns every successful DeleteTable, in order.
func (f *Fake) Deletions() []Deletion {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Deletion(nil), f.deletions...)
}

// CreateTableInputs returns the input of every successful CreateTable, in order.
func (f *Fake) CreateTableInputs() []dynamodb.CreateTableInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dynamodb.CreateTableInput(nil), f.creates...)
}

// Status reports a table's status, or "" if it does not exist. It does not
// advance CREATING or DELETING.
func (f *Fake) Status(name string) types.TableStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.tables[name]; ok {
		return t.status
	}
	return ""
}

// SetStatus forces an existing table's status, with countdown DescribeTable
// calls before it moves on (CREATING to ACTIVE, DELETING to gone). It is for
// starting a test from a table that is mid-transition.
func (f *Fake) SetStatus(name string, s types.TableStatus, countdown int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.tables[name]; ok {
		t.status, t.countdown = s, countdown
	}
}

// Item returns a copy of an item, by its hash key value, or nil.
func (f *Fake) Item(tableName, key string) map[string]types.AttributeValue {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.tables[tableName]; ok {
		return cloneItem(t.items[key])
	}
	return nil
}

// Keys returns the hash keys of every item in a table, sorted.
func (f *Fake) Keys(tableName string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.tables[tableName]; ok {
		return t.keys()
	}
	return nil
}

// PutRaw stores an item unconditionally, bypassing expressions. It is for
// seeding a test with a state the code under test would not write itself.
func (f *Fake) PutRaw(tableName string, item map[string]types.AttributeValue) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tables[tableName]
	if !ok {
		return notFound(tableName)
	}
	k, err := t.keyOf(item)
	if err != nil {
		return err
	}
	t.items[k] = cloneItem(item)
	return nil
}

func (t *table) keys() []string {
	out := make([]string, 0, len(t.items))
	for k := range t.items {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (t *table) keyOf(key map[string]types.AttributeValue) (string, error) {
	v, ok := key[t.key].(*types.AttributeValueMemberS)
	if !ok {
		return "", validation("The provided key element does not match the schema")
	}
	if v.Value == "" {
		return "", validation("One or more parameter values are not valid. The AttributeValue for a key attribute cannot contain an empty string value.")
	}
	return v.Value, nil
}

func notFound(name string) error {
	return &types.ResourceNotFoundException{Message: aws.String("Requested resource not found: Table: " + name + " not found")}
}

func validation(msg string) error {
	return &smithy.GenericAPIError{Code: "ValidationException", Message: msg, Fault: smithy.FaultClient}
}

// activeTable returns a table that accepts item operations. A table that is
// CREATING or DELETING does not.
func (f *Fake) activeTable(name *string) (*table, error) {
	n := aws.ToString(name)
	t, ok := f.tables[n]
	if !ok || t.status == types.TableStatusCreating || t.status == types.TableStatusDeleting {
		return nil, notFound(n)
	}
	return t, nil
}

// CreateTable creates a table with a single string hash key.
func (f *Fake) CreateTable(ctx context.Context, in *dynamodb.CreateTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error) {
	if err := f.begin(ctx, "CreateTable", in); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	name := aws.ToString(in.TableName)
	if _, ok := f.tables[name]; ok {
		return nil, &types.ResourceInUseException{Message: aws.String("Table already exists: " + name)}
	}
	if len(in.KeySchema) != 1 || in.KeySchema[0].KeyType != types.KeyTypeHash {
		return nil, validation("fake: only a single HASH key is supported")
	}
	t := &table{key: aws.ToString(in.KeySchema[0].AttributeName), status: types.TableStatusActive, items: map[string]map[string]types.AttributeValue{}}
	if f.CreateDelay > 0 {
		t.status, t.countdown = types.TableStatusCreating, f.CreateDelay
	}
	f.tables[name] = t
	f.creates = append(f.creates, *in)
	return &dynamodb.CreateTableOutput{TableDescription: &types.TableDescription{TableName: in.TableName, TableStatus: t.status}}, nil
}

// DescribeTable reports a table's status, advancing CREATING and DELETING.
func (f *Fake) DescribeTable(ctx context.Context, in *dynamodb.DescribeTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	if err := f.begin(ctx, "DescribeTable", in); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	name := aws.ToString(in.TableName)
	t, ok := f.tables[name]
	if !ok {
		return nil, notFound(name)
	}
	switch {
	case t.status == types.TableStatusCreating && t.countdown > 0:
		t.countdown--
	case t.status == types.TableStatusCreating:
		t.status = types.TableStatusActive
	case t.status == types.TableStatusDeleting && t.countdown > 0:
		t.countdown--
	case t.status == types.TableStatusDeleting:
		delete(f.tables, name)
		return nil, notFound(name)
	}
	return &dynamodb.DescribeTableOutput{Table: &types.TableDescription{TableName: in.TableName, TableStatus: t.status, ItemCount: aws.Int64(int64(len(t.items)))}}, nil
}

// DeleteTable deletes an ACTIVE table and everything in it.
func (f *Fake) DeleteTable(ctx context.Context, in *dynamodb.DeleteTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteTableOutput, error) {
	if err := f.begin(ctx, "DeleteTable", in); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	name := aws.ToString(in.TableName)
	t, ok := f.tables[name]
	if !ok {
		return nil, notFound(name)
	}
	if t.status != types.TableStatusActive {
		return nil, &types.ResourceInUseException{Message: aws.String("Attempt to change a resource which is still in use: Table is being " + string(t.status))}
	}
	f.deletions = append(f.deletions, Deletion{Table: name, Keys: t.keys()})
	t.items = map[string]map[string]types.AttributeValue{}
	if f.DeleteDelay > 0 {
		t.status, t.countdown = types.TableStatusDeleting, f.DeleteDelay
	} else {
		delete(f.tables, name)
	}
	return &dynamodb.DeleteTableOutput{TableDescription: &types.TableDescription{TableName: in.TableName, TableStatus: types.TableStatusDeleting}}, nil
}

// GetItem reads one item. Every read is strongly consistent.
func (f *Fake) GetItem(ctx context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	if err := f.begin(ctx, "GetItem", in); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t, err := f.activeTable(in.TableName)
	if err != nil {
		return nil, err
	}
	k, err := t.keyOf(in.Key)
	if err != nil {
		return nil, err
	}
	return &dynamodb.GetItemOutput{Item: cloneItem(t.items[k])}, nil
}

// Scan returns items in hash-key order, honouring Limit and ExclusiveStartKey.
// Every scan is strongly consistent.
func (f *Fake) Scan(ctx context.Context, in *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	if err := f.begin(ctx, "Scan", in); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t, err := f.activeTable(in.TableName)
	if err != nil {
		return nil, err
	}
	start := ""
	if in.ExclusiveStartKey != nil {
		if start, err = t.keyOf(in.ExclusiveStartKey); err != nil {
			return nil, err
		}
	}
	limit := f.ScanPage
	if in.Limit != nil {
		limit = int(*in.Limit)
	}
	out := &dynamodb.ScanOutput{}
	for _, k := range t.keys() {
		if start != "" && k <= start {
			continue
		}
		if limit > 0 && len(out.Items) == limit {
			out.LastEvaluatedKey = map[string]types.AttributeValue{t.key: &types.AttributeValueMemberS{Value: out.Items[len(out.Items)-1][t.key].(*types.AttributeValueMemberS).Value}}
			break
		}
		out.Items = append(out.Items, cloneItem(t.items[k]))
	}
	out.Count = int32(len(out.Items))
	return out, nil
}

// write is one prepared item write: evaluated against the current item, then
// applied.
type write struct {
	t     *table
	key   string
	cond  cond
	apply func(old map[string]types.AttributeValue) (map[string]types.AttributeValue, error) // nil result deletes
	ret   bool                                                                               // return the old item on condition failure
}

func (f *Fake) prepare(t *table, key map[string]types.AttributeValue, condExpr, updExpr *string, names map[string]string, values map[string]types.AttributeValue) (string, cond, *update, error) {
	k, err := t.keyOf(key)
	if err != nil {
		return "", nil, nil, err
	}
	if len(key) != 1 {
		return "", nil, nil, validation("The provided key element does not match the schema")
	}
	for k, v := range values {
		if err := validNumbers(v); err != nil {
			return "", nil, nil, validation("ExpressionAttributeValues contains invalid value: " + err.Error() + " for key " + k)
		}
	}
	ctx := newExprCtx(names, values)
	var c cond
	if condExpr != nil {
		if c, err = parseCondition(*condExpr, ctx); err != nil {
			return "", nil, nil, validation("Invalid ConditionExpression: " + err.Error())
		}
	}
	var u *update
	if updExpr != nil {
		if u, err = parseUpdate(*updExpr, ctx); err != nil {
			return "", nil, nil, validation("Invalid UpdateExpression: " + err.Error())
		}
		for _, p := range u.paths {
			if p[0] == t.key {
				return "", nil, nil, validation("One or more parameter values were invalid: Cannot update attribute " + t.key + ". This attribute is part of the key")
			}
		}
	}
	if err := ctx.checkAllUsed(); err != nil {
		return "", nil, nil, validation(err.Error())
	}
	return k, c, u, nil
}

// validNumbers rejects a malformed number anywhere in v, as DynamoDB does
// when it receives the request.
func validNumbers(v types.AttributeValue) error {
	switch x := v.(type) {
	case *types.AttributeValueMemberN:
		_, err := parseNum(x.Value)
		return err
	case *types.AttributeValueMemberNS:
		for _, e := range x.Value {
			if _, err := parseNum(e); err != nil {
				return err
			}
		}
	case *types.AttributeValueMemberM:
		for _, e := range x.Value {
			if err := validNumbers(e); err != nil {
				return err
			}
		}
	case *types.AttributeValueMemberL:
		for _, e := range x.Value {
			if err := validNumbers(e); err != nil {
				return err
			}
		}
	}
	return nil
}

// check evaluates w's condition against the current item.
func (w *write) check() (bool, error) {
	if w.cond == nil {
		return true, nil
	}
	old := w.t.items[w.key]
	if old == nil {
		old = map[string]types.AttributeValue{}
	}
	return w.cond(old)
}

func (w *write) commit() error {
	nu, err := w.apply(w.t.items[w.key])
	if err != nil {
		return err
	}
	if nu == nil {
		delete(w.t.items, w.key)
	} else {
		w.t.items[w.key] = nu
	}
	return nil
}

func updateApply(t *table, k string, u *update) func(old map[string]types.AttributeValue) (map[string]types.AttributeValue, error) {
	return func(old map[string]types.AttributeValue) (map[string]types.AttributeValue, error) {
		item := cloneItem(old)
		if item == nil {
			item = map[string]types.AttributeValue{t.key: &types.AttributeValueMemberS{Value: k}}
		}
		snapshot := cloneItem(item)
		for _, a := range u.actions {
			if err := a(item, snapshot); err != nil {
				return nil, validation(err.Error())
			}
		}
		return item, nil
	}
}

func putApply(item map[string]types.AttributeValue) func(map[string]types.AttributeValue) (map[string]types.AttributeValue, error) {
	return func(map[string]types.AttributeValue) (map[string]types.AttributeValue, error) {
		return cloneItem(item), nil
	}
}

func deleteApply(map[string]types.AttributeValue) (map[string]types.AttributeValue, error) {
	return nil, nil
}

func ccf(w *write) error {
	e := &types.ConditionalCheckFailedException{Message: aws.String("The conditional request failed")}
	if w.ret {
		e.Item = cloneItem(w.t.items[w.key])
	}
	return e
}

// single runs one non-transactional write.
func (f *Fake) single(w *write) error {
	ok, err := w.check()
	if err != nil {
		return validation(err.Error())
	}
	if !ok {
		return ccf(w)
	}
	return w.commit()
}

// PutItem writes a whole item, subject to ConditionExpression.
func (f *Fake) PutItem(ctx context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	if err := f.begin(ctx, "PutItem", in); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t, err := f.activeTable(in.TableName)
	if err != nil {
		return nil, err
	}
	k, c, _, err := f.prepare(t, map[string]types.AttributeValue{t.key: in.Item[t.key]}, in.ConditionExpression, nil, in.ExpressionAttributeNames, in.ExpressionAttributeValues)
	if err != nil {
		return nil, err
	}
	w := &write{t: t, key: k, cond: c, apply: putApply(in.Item), ret: in.ReturnValuesOnConditionCheckFailure == types.ReturnValuesOnConditionCheckFailureAllOld}
	return &dynamodb.PutItemOutput{}, f.single(w)
}

// UpdateItem applies UpdateExpression, subject to ConditionExpression. Like
// DynamoDB, it creates the item if it does not exist and the condition allows.
// ReturnValues ALL_NEW and ALL_OLD are supported.
func (f *Fake) UpdateItem(ctx context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	if err := f.begin(ctx, "UpdateItem", in); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t, err := f.activeTable(in.TableName)
	if err != nil {
		return nil, err
	}
	if in.UpdateExpression == nil {
		return nil, validation("fake: UpdateItem needs an UpdateExpression")
	}
	k, c, u, err := f.prepare(t, in.Key, in.ConditionExpression, in.UpdateExpression, in.ExpressionAttributeNames, in.ExpressionAttributeValues)
	if err != nil {
		return nil, err
	}
	w := &write{t: t, key: k, cond: c, apply: updateApply(t, k, u), ret: in.ReturnValuesOnConditionCheckFailure == types.ReturnValuesOnConditionCheckFailureAllOld}
	old := cloneItem(t.items[k])
	if err := f.single(w); err != nil {
		return nil, err
	}
	out := &dynamodb.UpdateItemOutput{}
	switch in.ReturnValues {
	case types.ReturnValueAllNew:
		out.Attributes = cloneItem(t.items[k])
	case types.ReturnValueAllOld:
		out.Attributes = old
	}
	return out, nil
}

// DeleteItem deletes one item, subject to ConditionExpression. Deleting an item
// that does not exist succeeds, as in DynamoDB.
func (f *Fake) DeleteItem(ctx context.Context, in *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	if err := f.begin(ctx, "DeleteItem", in); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t, err := f.activeTable(in.TableName)
	if err != nil {
		return nil, err
	}
	k, c, _, err := f.prepare(t, in.Key, in.ConditionExpression, nil, in.ExpressionAttributeNames, in.ExpressionAttributeValues)
	if err != nil {
		return nil, err
	}
	w := &write{t: t, key: k, cond: c, apply: deleteApply, ret: in.ReturnValuesOnConditionCheckFailure == types.ReturnValuesOnConditionCheckFailureAllOld}
	return &dynamodb.DeleteItemOutput{}, f.single(w)
}

// TransactWriteItems applies every write or none. Every condition is evaluated
// against the state before the transaction; if any fails, the result is a
// TransactionCanceledException whose CancellationReasons line up with the
// request, "ConditionalCheckFailed" for each failed condition and "None" for
// the rest.
func (f *Fake) TransactWriteItems(ctx context.Context, in *dynamodb.TransactWriteItemsInput, _ ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	if err := f.begin(ctx, "TransactWriteItems", in); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(in.TransactItems) == 0 || len(in.TransactItems) > 100 {
		return nil, validation("1 to 100 TransactItems are required")
	}
	ws := make([]*write, 0, len(in.TransactItems))
	seen := map[string]bool{}
	for _, ti := range in.TransactItems {
		w, err := f.prepareTx(ti)
		if err != nil {
			return nil, err
		}
		id := fmt.Sprintf("%p/%s", w.t, w.key)
		if seen[id] {
			return nil, validation("Transaction request cannot include multiple operations on one item")
		}
		seen[id] = true
		ws = append(ws, w)
	}
	reasons := make([]types.CancellationReason, len(ws))
	failed := false
	for i, w := range ws {
		ok, err := w.check()
		if err != nil {
			return nil, validation(err.Error())
		}
		if ok {
			reasons[i] = types.CancellationReason{Code: aws.String("None")}
			continue
		}
		failed = true
		reasons[i] = types.CancellationReason{Code: aws.String("ConditionalCheckFailed"), Message: aws.String("The conditional request failed")}
		if w.ret {
			reasons[i].Item = cloneItem(w.t.items[w.key])
		}
	}
	if failed {
		return nil, &types.TransactionCanceledException{Message: aws.String("Transaction cancelled, please refer cancellation reasons for specific reasons"), CancellationReasons: reasons}
	}
	// Apply against copies first so a validation error in one write cannot
	// leave the transaction half applied.
	next := make([]map[string]types.AttributeValue, len(ws))
	for i, w := range ws {
		nu, err := w.apply(w.t.items[w.key])
		if err != nil {
			return nil, err
		}
		next[i] = nu
	}
	for i, w := range ws {
		if next[i] == nil {
			delete(w.t.items, w.key)
		} else {
			w.t.items[w.key] = next[i]
		}
	}
	return &dynamodb.TransactWriteItemsOutput{}, nil
}

func (f *Fake) prepareTx(ti types.TransactWriteItem) (*write, error) {
	n := 0
	for _, set := range []bool{ti.ConditionCheck != nil, ti.Put != nil, ti.Update != nil, ti.Delete != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		return nil, validation("each TransactWriteItem needs exactly one of ConditionCheck, Put, Update, Delete")
	}
	all := types.ReturnValuesOnConditionCheckFailureAllOld
	switch {
	case ti.ConditionCheck != nil:
		cc := ti.ConditionCheck
		t, err := f.activeTable(cc.TableName)
		if err != nil {
			return nil, err
		}
		if cc.ConditionExpression == nil {
			return nil, validation("ConditionCheck needs a ConditionExpression")
		}
		k, c, _, err := f.prepare(t, cc.Key, cc.ConditionExpression, nil, cc.ExpressionAttributeNames, cc.ExpressionAttributeValues)
		if err != nil {
			return nil, err
		}
		return &write{t: t, key: k, cond: c, ret: cc.ReturnValuesOnConditionCheckFailure == all,
			apply: func(old map[string]types.AttributeValue) (map[string]types.AttributeValue, error) {
				return cloneItem(old), nil
			}}, nil
	case ti.Put != nil:
		p := ti.Put
		t, err := f.activeTable(p.TableName)
		if err != nil {
			return nil, err
		}
		k, c, _, err := f.prepare(t, map[string]types.AttributeValue{t.key: p.Item[t.key]}, p.ConditionExpression, nil, p.ExpressionAttributeNames, p.ExpressionAttributeValues)
		if err != nil {
			return nil, err
		}
		return &write{t: t, key: k, cond: c, apply: putApply(p.Item), ret: p.ReturnValuesOnConditionCheckFailure == all}, nil
	case ti.Update != nil:
		u := ti.Update
		t, err := f.activeTable(u.TableName)
		if err != nil {
			return nil, err
		}
		if u.UpdateExpression == nil {
			return nil, validation("Update needs an UpdateExpression")
		}
		k, c, up, err := f.prepare(t, u.Key, u.ConditionExpression, u.UpdateExpression, u.ExpressionAttributeNames, u.ExpressionAttributeValues)
		if err != nil {
			return nil, err
		}
		return &write{t: t, key: k, cond: c, apply: updateApply(t, k, up), ret: u.ReturnValuesOnConditionCheckFailure == all}, nil
	default:
		d := ti.Delete
		t, err := f.activeTable(d.TableName)
		if err != nil {
			return nil, err
		}
		k, c, _, err := f.prepare(t, d.Key, d.ConditionExpression, nil, d.ExpressionAttributeNames, d.ExpressionAttributeValues)
		if err != nil {
			return nil, err
		}
		return &write{t: t, key: k, cond: c, apply: deleteApply, ret: d.ReturnValuesOnConditionCheckFailure == all}, nil
	}
}
