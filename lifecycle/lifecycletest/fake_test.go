package lifecycletest

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"
)

var ctx = context.Background()

type av = types.AttributeValue

func S(v string) av     { return &types.AttributeValueMemberS{Value: v} }
func N(v string) av     { return &types.AttributeValueMemberN{Value: v} }
func SS(v ...string) av { return &types.AttributeValueMemberSS{Value: v} }
func M(m map[string]av) av {
	return &types.AttributeValueMemberM{Value: m}
}

var item = map[string]av{
	"pk":    S("k"),
	"name":  S("alpha"),
	"count": N("10"),
	"set":   SS("b", "a"),
	"flag":  &types.AttributeValueMemberBOOL{Value: true},
	"lease": M(map[string]av{"token": S("t1"), "hb": S("2026-01-01T00:00:00.000Z")}),
}

func evalCond(t *testing.T, expr string, names map[string]string, vals map[string]av) (bool, error) {
	t.Helper()
	ec := newExprCtx(names, vals)
	c, err := parseCondition(expr, ec)
	if err != nil {
		return false, err
	}
	if err := ec.checkAllUsed(); err != nil {
		return false, err
	}
	return c(item)
}

func TestConditionSemantics(t *testing.T) {
	nm := map[string]string{"#l": "lease", "#t": "token", "#n": "name", "#c": "count", "#s": "set", "#x": "missing", "#f": "flag"}
	for _, c := range []struct {
		expr string
		want bool
	}{
		{"attribute_exists(#n)", true},
		{"attribute_not_exists(#n)", false},
		{"attribute_not_exists(#x)", true},
		{"attribute_exists(#l.#t)", true},
		{"attribute_exists(#n.#t)", false}, // path through a non-map
		{"#l.#t = :t1", true},
		{"#l.#t = :other", false},
		{"#n = :alpha", true},
		{"#n < :beta", true},
		{"#n <= :alpha", true},
		{"#n > :alpha", false},
		{"#n >= :alpha", true},
		{"#c > :nine", true}, // numeric, not lexicographic ("10" < "9" as strings)
		{"#c = :ten", true},
		{"#c < :nine", false},
		{"#x = :alpha", false}, // missing attribute: false
		{"#x < :alpha", false}, // missing attribute: false
		{"#c = :alpha", false}, // type mismatch: false
		{"#n < :nine", false},  // type mismatch: false
		{"#s = :ab", true},     // sets compare unordered
		{"#s < :ab", false},    // sets have no order
		{"#f = :true", true},   // BOOL equality
		{"begins_with(#n, :al)", true},
		{"begins_with(#n, :beta)", false},
		{"begins_with(#x, :al)", false},
		{"contains(#n, :lph)", true},
		{"contains(#s, :a)", true},
		{"contains(#s, :beta)", false},
		{"contains(#s, :nine)", false},
		{"contains(#c, :a)", false},
		{"NOT attribute_exists(#x)", true},
		{"attribute_exists(#x) OR #n = :alpha", true},
		{"attribute_exists(#x) AND #n = :alpha", false},
		{"#n = :alpha OR attribute_exists(#x) AND #n = :beta", true}, // AND binds tighter
		{"(#n = :alpha OR attribute_exists(#x)) AND #n = :beta", false},
		{"NOT (#n = :alpha)", false},
		{"pk = :k", true}, // bare identifier
	} {
		vals := map[string]av{}
		for _, v := range []struct {
			k string
			a av
		}{{":t1", S("t1")}, {":other", S("o")}, {":alpha", S("alpha")}, {":beta", S("beta")}, {":nine", N("9")}, {":ten", N("10.0")},
			{":ab", SS("a", "b")}, {":true", &types.AttributeValueMemberBOOL{Value: true}}, {":al", S("al")}, {":lph", S("lph")}, {":a", S("a")}, {":k", S("k")}} {
			if usesToken(c.expr, v.k) {
				vals[v.k] = v.a
			}
		}
		used := map[string]string{}
		for k, v := range nm {
			if usesToken(c.expr, k) {
				used[k] = v
			}
		}
		got, err := evalCond(t, c.expr, used, vals)
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s = %v, want %v", c.expr, got, c.want)
		}
	}
}

func TestConditionRejections(t *testing.T) {
	for _, c := range []struct {
		expr  string
		names map[string]string
		vals  map[string]av
		want  string
	}{
		{"#n = :v", nil, map[string]av{":v": S("x")}, "attribute name used in the document path is not defined"},
		{"name = :v", nil, nil, "attribute value used in expression is not defined"},
		{"name = :v", map[string]string{"#unused": "x"}, map[string]av{":v": S("x")}, "ExpressionAttributeNames unused"},
		{"name = :v", nil, map[string]av{":v": S("x"), ":w": S("y")}, "ExpressionAttributeValues unused"},
		{"name <> :v", nil, map[string]av{":v": S("x")}, "not supported"},
		{"size(name) = :v", nil, map[string]av{":v": S("x")}, "not supported"},
		{"name = if_not_exists(name, :v)", nil, map[string]av{":v": S("x")}, "not supported"},
		{"name :v", nil, map[string]av{":v": S("x")}, "expected a comparator"},
		{"name = :v extra", nil, map[string]av{":v": S("x")}, "unexpected"},
		{"(name = :v", nil, map[string]av{":v": S("x")}, "expected )"},
		{"name[0] = :v", nil, map[string]av{":v": S("x")}, "unsupported character"},
		{"9name = :v", nil, map[string]av{":v": S("x")}, "invalid token"},
		{"# = :v", nil, map[string]av{":v": S("x")}, "invalid token"},
		{"= :v", nil, map[string]av{":v": S("x")}, "expected an attribute path"},
		{"begins_with(name :v)", nil, map[string]av{":v": S("x")}, "expected ,"},
		{"begins_with(name, :v", nil, map[string]av{":v": S("x")}, "expected )"},
		{"attribute_exists(:v)", nil, map[string]av{":v": S("x")}, "expected an attribute path"},
		{"begins_with(name, :w)", nil, map[string]av{":v": S("x")}, "not defined"},
		{"NOT", nil, nil, "expected an attribute path"},
		{"name = :v AND", nil, map[string]av{":v": S("x")}, "expected an attribute path"},
		{"name = :v OR", nil, map[string]av{":v": S("x")}, "expected an attribute path"},
		{"name = #x", nil, map[string]av{":v": S("x")}, "not defined"},
		{"NOT #x", nil, nil, "not defined"},
		{"(#x = :v)", nil, map[string]av{":v": S("x")}, "not defined"},
	} {
		_, err := evalCond(t, c.expr, c.names, c.vals)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: error %v, want %q", c.expr, err, c.want)
		}
	}
	// A malformed number is an error at evaluation time.
	ec := newExprCtx(nil, map[string]av{":v": N("x")})
	cnd, err := parseCondition("count < :v", ec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cnd(item); err == nil {
		t.Error("comparison with a malformed number succeeded")
	}
	cnd, _ = parseCondition("count < :v", newExprCtx(nil, map[string]av{":v": N("1")}))
	if _, err := cnd(map[string]av{"count": N("bad")}); err == nil {
		t.Error("comparison against a malformed stored number succeeded")
	}
}

func applyUpdate(t *testing.T, old map[string]av, expr string, names map[string]string, vals map[string]av) (map[string]av, error) {
	t.Helper()
	ec := newExprCtx(names, vals)
	u, err := parseUpdate(expr, ec)
	if err != nil {
		return nil, err
	}
	if err := ec.checkAllUsed(); err != nil {
		return nil, err
	}
	tb := &table{key: "pk"}
	return updateApply(tb, "k", u)(old)
}

func TestUpdateSemantics(t *testing.T) {
	got, err := applyUpdate(t, item,
		"SET #c = if_not_exists(#c, :zero) + :one, fresh = if_not_exists(nope, :zero) - :one, #l.#t = :t2, copy = #n, name = :new "+
			"REMOVE flag, #l.hb, nothing.deeper ADD #s :c, total :one, added :c DELETE gone :c",
		map[string]string{"#c": "count", "#l": "lease", "#t": "token", "#s": "set", "#n": "name"},
		map[string]av{":zero": N("0"), ":one": N("1"), ":t2": S("t2"), ":new": S("beta"), ":c": SS("c")})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]av{
		"pk":    S("k"),
		"name":  S("beta"),
		"copy":  S("alpha"), // operands see the item as it was before the update
		"count": N("11"),
		"fresh": N("-1"),
		"set":   SS("a", "b", "c"),
		"total": N("1"),
		"added": SS("c"),
		"lease": M(map[string]av{"token": S("t2")}),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("update result\n got %v\nwant %v", got, want)
	}
	if item["count"].(*types.AttributeValueMemberN).Value != "10" {
		t.Error("update mutated the input item")
	}

	// ADD to an existing number, DELETE down to an empty set removes it.
	got, err = applyUpdate(t, item, "ADD #c :two DELETE #s :ab", map[string]string{"#c": "count", "#s": "set"},
		map[string]av{":two": N("2.5"), ":ab": SS("a", "b")})
	if err != nil {
		t.Fatal(err)
	}
	if got["count"].(*types.AttributeValueMemberN).Value != "12.50000000000000000000" {
		t.Errorf("ADD 2.5 to 10 = %v", got["count"])
	}
	if _, ok := got["set"]; ok {
		t.Error("DELETE of every member left an empty set")
	}
	got, _ = applyUpdate(t, item, "DELETE #s :a", map[string]string{"#s": "set"}, map[string]av{":a": SS("a")})
	if !reflect.DeepEqual(got["set"], SS("b")) {
		t.Errorf("DELETE a from {a,b} = %v", got["set"])
	}
	// A new item gets its key.
	got, err = applyUpdate(t, nil, "SET a = :v", nil, map[string]av{":v": S("x")})
	if err != nil || !reflect.DeepEqual(got, map[string]av{"pk": S("k"), "a": S("x")}) {
		t.Errorf("update of a missing item = %v, %v", got, err)
	}
}

func TestUpdateRejections(t *testing.T) {
	for _, c := range []struct {
		expr string
		vals map[string]av
		want string
	}{
		{"SET lease = :v REMOVE lease.token", map[string]av{":v": S("x")}, "overlap"},
		{"SET a = :v, a = :v", map[string]av{":v": S("x")}, "overlap"},
		{"SET a = :v SET b = :v", map[string]av{":v": S("x")}, "only be used once"},
		{"SET a = :v + :v + :v", map[string]av{":v": N("1")}, "expected SET, REMOVE, ADD or DELETE"},
		{"UPSERT a = :v", map[string]av{":v": S("x")}, "expected SET"},
		{"SET a :v", map[string]av{":v": S("x")}, "expected = after SET"},
		{"SET a = if_not_exists(:v, :v)", map[string]av{":v": S("x")}, "expected an attribute path"},
		{"SET a = if_not_exists(b :v)", map[string]av{":v": S("x")}, "expected ,"},
		{"SET a = if_not_exists(b, :v", map[string]av{":v": S("x")}, "expected )"},
		{"SET a = if_not_exists(b, :w)", map[string]av{":v": S("x")}, "not defined"},
		{"SET a = :v + :w", map[string]av{":v": S("x")}, "not defined"},
		{"SET a = :w", map[string]av{":v": S("x")}, "not defined"},
		{"ADD a b", nil, "needs a :value"},
		{"ADD a :w", map[string]av{":v": S("x")}, "not defined"},
		{"ADD a.b :v", map[string]av{":v": N("1")}, "nested path"},
		{"REMOVE #x", nil, "not defined"},
		{"", nil, "empty update"},
		{"SET a = %", nil, "unsupported character"},
	} {
		_, err := applyUpdate(t, item, c.expr, nil, c.vals)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: error %v, want %q", c.expr, err, c.want)
		}
	}
	// Errors found while applying.
	for _, c := range []struct {
		expr string
		vals map[string]av
		want string
	}{
		{"SET lease.x.y = :v", map[string]av{":v": S("x")}, "invalid for update"},
		{"SET name.x = :v", map[string]av{":v": S("x")}, "invalid for update"},
		{"SET a = nope", nil, "does not exist"},
		{"SET a = nope + :v", map[string]av{":v": N("1")}, "does not exist"},
		{"SET a = name + :v", map[string]av{":v": N("1")}, "incorrect data type"},
		{"SET a = count - :v", map[string]av{":v": N("x")}, "invalid number"},
		{"SET a = :v - count", map[string]av{":v": N("x")}, "invalid number"},
		{"ADD name :v", map[string]av{":v": N("1")}, "incorrect data type"},
		{"ADD name :v", map[string]av{":v": SS("1")}, "incorrect data type"},
		{"ADD a :v", map[string]av{":v": S("1")}, "Incorrect operand type"},
		{"DELETE a :v", map[string]av{":v": S("1")}, "Incorrect operand type"},
		{"DELETE name :v", map[string]av{":v": SS("1")}, "incorrect data type"},
	} {
		_, err := applyUpdate(t, item, c.expr, nil, c.vals)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: error %v, want %q", c.expr, err, c.want)
		}
	}
	// A SET operand that is itself an error propagates.
	_, err := applyUpdate(t, map[string]av{"pk": S("k"), "n": N("bad")}, "SET a = n + :v", nil, map[string]av{":v": N("1")})
	if err == nil {
		t.Error("arithmetic on a malformed stored number succeeded")
	}
	// DELETE from a missing set is a no-op.
	got, err := applyUpdate(t, item, "DELETE nope :v", nil, map[string]av{":v": SS("x")})
	if err != nil || got["nope"] != nil {
		t.Errorf("DELETE from a missing set = %v, %v", got["nope"], err)
	}
}

func TestCloneAttr(t *testing.T) {
	in := []av{
		S("a"), N("1"), SS("a"), &types.AttributeValueMemberNS{Value: []string{"1"}},
		&types.AttributeValueMemberL{Value: []av{S("x"), M(map[string]av{"k": S("v")})}},
		&types.AttributeValueMemberBOOL{Value: true}, &types.AttributeValueMemberNULL{Value: true},
		M(map[string]av{"k": SS("v")}),
	}
	for _, v := range in {
		if c := cloneAttr(v); !reflect.DeepEqual(c, v) {
			t.Errorf("cloneAttr(%#v) = %#v", v, c)
		}
	}
	if cloneItem(nil) != nil {
		t.Error("cloneItem(nil) != nil")
	}
	// A clone is independent of its source.
	src := SS("a")
	c := cloneAttr(src).(*types.AttributeValueMemberSS)
	c.Value[0] = "z"
	if src.(*types.AttributeValueMemberSS).Value[0] != "a" {
		t.Error("cloneAttr shares the slice")
	}
	if formatNum(mustRat(t, "3/2")) != "1.50000000000000000000" {
		t.Error("formatNum of a fraction")
	}
}

// usesToken reports whether expr contains the #name or :value token tok.
func usesToken(expr, tok string) bool {
	toks, _ := lex(expr)
	for _, x := range toks {
		if x.text == tok {
			return true
		}
	}
	return false
}

func mustRat(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, err := parseNum(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// --- the Fake's operations ---

const tbl = "t"

func newTable(t *testing.T, f *Fake) {
	t.Helper()
	_, err := f.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(tbl),
		KeySchema: []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func key(k string) map[string]av { return map[string]av{"pk": S(k)} }

func isErr[T error](err error) bool {
	var e T
	return errors.As(err, &e)
}

func isValidation(err error) bool {
	var e smithy.APIError
	return errors.As(err, &e) && e.ErrorCode() == "ValidationException"
}

func TestTableLifecycle(t *testing.T) {
	f := New()
	f.CreateDelay, f.DeleteDelay = 1, 1
	if _, err := f.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(tbl)}); !isErr[*types.ResourceNotFoundException](err) {
		t.Errorf("describe missing = %v", err)
	}
	if _, err := f.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String(tbl)}); !isErr[*types.ResourceNotFoundException](err) {
		t.Errorf("delete missing = %v", err)
	}
	if _, err := f.CreateTable(ctx, &dynamodb.CreateTableInput{TableName: aws.String(tbl)}); !isValidation(err) {
		t.Errorf("create without a key schema = %v", err)
	}
	newTable(t, f)
	if _, err := f.CreateTable(ctx, &dynamodb.CreateTableInput{TableName: aws.String(tbl)}); !isErr[*types.ResourceInUseException](err) {
		t.Errorf("create existing = %v", err)
	}
	if f.Status(tbl) != types.TableStatusCreating || len(f.CreateTableInputs()) != 1 {
		t.Errorf("status after create = %s", f.Status(tbl))
	}
	if _, err := f.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(tbl), Key: key("a")}); !isErr[*types.ResourceNotFoundException](err) {
		t.Errorf("item op on a CREATING table = %v", err)
	}
	if _, err := f.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String(tbl)}); !isErr[*types.ResourceInUseException](err) {
		t.Errorf("delete of a CREATING table = %v", err)
	}
	for _, want := range []types.TableStatus{types.TableStatusCreating, types.TableStatusActive, types.TableStatusActive} {
		out, err := f.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(tbl)})
		if err != nil || out.Table.TableStatus != want {
			t.Fatalf("describe = %v, %v, want %s", out, err, want)
		}
	}
	if err := f.PutRaw(tbl, map[string]av{"pk": S("a")}); err != nil {
		t.Fatal(err)
	}
	if err := f.PutRaw(tbl, map[string]av{"x": S("a")}); err == nil {
		t.Error("PutRaw without a key succeeded")
	}
	if err := f.PutRaw("nope", key("a")); err == nil {
		t.Error("PutRaw to a missing table succeeded")
	}
	if _, err := f.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String(tbl)}); err != nil {
		t.Fatal(err)
	}
	if d := f.Deletions(); len(d) != 1 || !reflect.DeepEqual(d[0].Keys, []string{"a"}) {
		t.Errorf("deletions = %+v", d)
	}
	if f.Status(tbl) != types.TableStatusDeleting {
		t.Errorf("status after delete = %s", f.Status(tbl))
	}
	if _, err := f.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(tbl)}); err != nil {
		t.Errorf("describe while DELETING = %v", err)
	}
	if _, err := f.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(tbl)}); !isErr[*types.ResourceNotFoundException](err) {
		t.Errorf("describe after the delete completed = %v", err)
	}
	if f.Status(tbl) != "" || f.Keys(tbl) != nil || f.Item(tbl, "a") != nil {
		t.Error("table still visible after deletion")
	}
	f.SetStatus("nope", types.TableStatusActive, 0) // no table: no effect
	if f.Calls("DescribeTable") != 6 {
		t.Errorf("Calls(DescribeTable) = %d", f.Calls("DescribeTable"))
	}
}

func TestItemOperations(t *testing.T) {
	f := New()
	newTable(t, f)
	hooked := 0
	f.Hook = func(context.Context, string) { hooked++ }
	put := func(k string, cond string, vals map[string]av, ret bool) error {
		in := &dynamodb.PutItemInput{TableName: aws.String(tbl), Item: map[string]av{"pk": S(k), "v": S("1")}, ExpressionAttributeValues: vals}
		if cond != "" {
			in.ConditionExpression = aws.String(cond)
		}
		if ret {
			in.ReturnValuesOnConditionCheckFailure = types.ReturnValuesOnConditionCheckFailureAllOld
		}
		_, err := f.PutItem(ctx, in)
		return err
	}
	if err := put("a", "attribute_not_exists(pk)", nil, false); err != nil {
		t.Fatal(err)
	}
	err := put("a", "attribute_not_exists(pk)", nil, true)
	var ccf *types.ConditionalCheckFailedException
	if !errors.As(err, &ccf) || ccf.Item["v"] == nil {
		t.Errorf("conditional put over an existing item = %v", err)
	}
	if err := put("a", "v = :x", map[string]av{":x": S("1"), ":y": S("2")}, false); !isValidation(err) {
		t.Errorf("put with an unused value = %v", err)
	}
	if err := put("a", "v = ", nil, false); !isValidation(err) {
		t.Errorf("put with a bad condition = %v", err)
	}
	if err := put("", "", nil, false); !isValidation(err) {
		t.Errorf("put with an empty key = %v", err)
	}

	out, err := f.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(tbl), Key: key("a"), UpdateExpression: aws.String("SET v = :two"),
		ConditionExpression: aws.String("v = :one"), ExpressionAttributeValues: map[string]av{":one": S("1"), ":two": S("2")},
		ReturnValues: types.ReturnValueAllOld,
	})
	if err != nil || !reflect.DeepEqual(out.Attributes["v"], S("1")) {
		t.Errorf("update ALL_OLD = %v, %v", out, err)
	}
	out, err = f.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(tbl), Key: key("a"), UpdateExpression: aws.String("SET w = :w"),
		ExpressionAttributeValues: map[string]av{":w": S("w")}, ReturnValues: types.ReturnValueAllNew,
	})
	if err != nil || !reflect.DeepEqual(out.Attributes["v"], S("2")) || !reflect.DeepEqual(out.Attributes["w"], S("w")) {
		t.Errorf("update ALL_NEW = %v, %v", out, err)
	}
	// A failed condition does not create the item.
	_, err = f.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(tbl), Key: key("new"), UpdateExpression: aws.String("SET v = :v"),
		ConditionExpression: aws.String("attribute_exists(pk)"), ExpressionAttributeValues: map[string]av{":v": S("x")},
	})
	if !isErr[*types.ConditionalCheckFailedException](err) || f.Item(tbl, "new") != nil {
		t.Errorf("conditional update of a missing item = %v", err)
	}
	for _, in := range []*dynamodb.UpdateItemInput{
		{TableName: aws.String(tbl), Key: key("a")},
		{TableName: aws.String(tbl), Key: key("a"), UpdateExpression: aws.String("SET pk = :v"), ExpressionAttributeValues: map[string]av{":v": S("x")}},
		{TableName: aws.String(tbl), Key: map[string]av{"pk": S("a"), "x": S("y")}, UpdateExpression: aws.String("REMOVE v")},
		{TableName: aws.String(tbl), Key: map[string]av{"pk": N("1")}, UpdateExpression: aws.String("REMOVE v")},
		{TableName: aws.String(tbl), Key: key("a"), UpdateExpression: aws.String("SET v = :v"), ConditionExpression: aws.String("v <> :v"), ExpressionAttributeValues: map[string]av{":v": S("x")}},
		{TableName: aws.String(tbl), Key: key("a"), UpdateExpression: aws.String("SET v = nope")},
		{TableName: aws.String(tbl), Key: key("a"), UpdateExpression: aws.String("SET v = :v"), ConditionExpression: aws.String("w < :n"), ExpressionAttributeValues: map[string]av{":v": S("x"), ":n": N("bad")}},
	} {
		if _, err := f.UpdateItem(ctx, in); !isValidation(err) {
			t.Errorf("update %s = %v, want ValidationException", aws.ToString(in.UpdateExpression), err)
		}
	}

	g, err := f.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(tbl), Key: key("a")})
	if err != nil || !reflect.DeepEqual(g.Item["w"], S("w")) {
		t.Errorf("get = %v, %v", g, err)
	}
	if _, err := f.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(tbl), Key: key("")}); !isValidation(err) {
		t.Errorf("get with an empty key = %v", err)
	}

	_, err = f.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: aws.String(tbl), Key: key("a"), ConditionExpression: aws.String("v = :x"),
		ExpressionAttributeValues: map[string]av{":x": S("nope")}, ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld})
	if !errors.As(err, &ccf) || ccf.Item == nil {
		t.Errorf("conditional delete = %v", err)
	}
	if _, err := f.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: aws.String(tbl), Key: key("a")}); err != nil || f.Item(tbl, "a") != nil {
		t.Errorf("delete = %v", err)
	}
	if _, err := f.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: aws.String(tbl), Key: key("a")}); err != nil {
		t.Errorf("delete of a missing item = %v", err)
	}
	if _, err := f.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: aws.String(tbl), Key: key("")}); !isValidation(err) {
		t.Errorf("delete with an empty key = %v", err)
	}
	if hooked == 0 {
		t.Error("Hook never called")
	}

	// Operations on a missing table.
	missing := aws.String("nope")
	if _, err := f.GetItem(ctx, &dynamodb.GetItemInput{TableName: missing, Key: key("a")}); !isErr[*types.ResourceNotFoundException](err) {
		t.Error(err)
	}
	if _, err := f.PutItem(ctx, &dynamodb.PutItemInput{TableName: missing, Item: key("a")}); !isErr[*types.ResourceNotFoundException](err) {
		t.Error(err)
	}
	if _, err := f.UpdateItem(ctx, &dynamodb.UpdateItemInput{TableName: missing, Key: key("a")}); !isErr[*types.ResourceNotFoundException](err) {
		t.Error(err)
	}
	if _, err := f.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: missing, Key: key("a")}); !isErr[*types.ResourceNotFoundException](err) {
		t.Error(err)
	}
	if _, err := f.Scan(ctx, &dynamodb.ScanInput{TableName: missing}); !isErr[*types.ResourceNotFoundException](err) {
		t.Error(err)
	}
}

func TestErrInjection(t *testing.T) {
	f := New()
	boom := errors.New("boom")
	f.Err = func(op string, _ any) error { return boom }
	calls := []func() error{
		func() error { _, err := f.CreateTable(ctx, &dynamodb.CreateTableInput{}); return err },
		func() error { _, err := f.DescribeTable(ctx, &dynamodb.DescribeTableInput{}); return err },
		func() error { _, err := f.DeleteTable(ctx, &dynamodb.DeleteTableInput{}); return err },
		func() error { _, err := f.GetItem(ctx, &dynamodb.GetItemInput{}); return err },
		func() error { _, err := f.PutItem(ctx, &dynamodb.PutItemInput{}); return err },
		func() error { _, err := f.UpdateItem(ctx, &dynamodb.UpdateItemInput{}); return err },
		func() error { _, err := f.DeleteItem(ctx, &dynamodb.DeleteItemInput{}); return err },
		func() error { _, err := f.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{}); return err },
		func() error { _, err := f.Scan(ctx, &dynamodb.ScanInput{}); return err },
	}
	for i, c := range calls {
		if err := c(); !errors.Is(err, boom) {
			t.Errorf("call %d = %v, want the injected error", i, err)
		}
	}
}

func TestScanPagination(t *testing.T) {
	f := New()
	newTable(t, f)
	for _, k := range []string{"c", "a", "b"} {
		if err := f.PutRaw(tbl, key(k)); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	var start map[string]av
	pages := 0
	for {
		out, err := f.Scan(ctx, &dynamodb.ScanInput{TableName: aws.String(tbl), Limit: aws.Int32(2), ExclusiveStartKey: start})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, it := range out.Items {
			got = append(got, it["pk"].(*types.AttributeValueMemberS).Value)
		}
		if out.LastEvaluatedKey == nil {
			break
		}
		start = out.LastEvaluatedKey
	}
	if !reflect.DeepEqual(got, []string{"a", "b", "c"}) || pages != 2 {
		t.Errorf("scan = %v in %d pages", got, pages)
	}
	f.ScanPage = 1
	out, _ := f.Scan(ctx, &dynamodb.ScanInput{TableName: aws.String(tbl)})
	if len(out.Items) != 1 || out.LastEvaluatedKey == nil {
		t.Errorf("ScanPage=1 gave %d items", len(out.Items))
	}
	for _, v := range []av{&types.AttributeValueMemberNS{Value: []string{"1", "x"}}, M(map[string]av{"n": N("x")}), &types.AttributeValueMemberL{Value: []av{N("x")}}} {
		if err := validNumbers(v); err == nil {
			t.Errorf("validNumbers(%v) accepted a malformed number", v)
		}
	}
	if err := validNumbers(M(map[string]av{"l": &types.AttributeValueMemberL{Value: []av{&types.AttributeValueMemberNS{Value: []string{"1"}}}}})); err != nil {
		t.Errorf("validNumbers of good numbers = %v", err)
	}
	if _, err := f.Scan(ctx, &dynamodb.ScanInput{TableName: aws.String(tbl), ExclusiveStartKey: map[string]av{"pk": N("1")}}); !isValidation(err) {
		t.Errorf("scan with a bad start key = %v", err)
	}
	if !reflect.DeepEqual(f.Keys(tbl), []string{"a", "b", "c"}) {
		t.Errorf("Keys = %v", f.Keys(tbl))
	}
}

func TestTransactWriteItems(t *testing.T) {
	f := New()
	newTable(t, f)
	if err := f.PutRaw(tbl, map[string]av{"pk": S("a"), "v": N("1")}); err != nil {
		t.Fatal(err)
	}
	tx := func(items ...types.TransactWriteItem) error {
		_, err := f.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: items})
		return err
	}
	check := func(k, cond string) types.TransactWriteItem {
		return types.TransactWriteItem{ConditionCheck: &types.ConditionCheck{TableName: aws.String(tbl), Key: key(k), ConditionExpression: aws.String(cond),
			ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld}}
	}
	upd := func(k, expr string) types.TransactWriteItem {
		return types.TransactWriteItem{Update: &types.Update{TableName: aws.String(tbl), Key: key(k), UpdateExpression: aws.String(expr),
			ExpressionAttributeValues: map[string]av{":one": N("1")}}}
	}
	put := types.TransactWriteItem{Put: &types.Put{TableName: aws.String(tbl), Item: map[string]av{"pk": S("p")}, ConditionExpression: aws.String("attribute_not_exists(pk)")}}
	del := types.TransactWriteItem{Delete: &types.Delete{TableName: aws.String(tbl), Key: key("d")}}

	// All pass: everything applies.
	if err := tx(check("a", "attribute_exists(pk)"), upd("b", "ADD v :one"), put, del); err != nil {
		t.Fatal(err)
	}
	if f.Item(tbl, "b") == nil || f.Item(tbl, "p") == nil {
		t.Error("transaction did not apply")
	}
	// One fails: nothing applies, and the reasons line up.
	err := tx(check("a", "attribute_not_exists(pk)"), upd("c", "ADD v :one"))
	var tce *types.TransactionCanceledException
	if !errors.As(err, &tce) || len(tce.CancellationReasons) != 2 {
		t.Fatalf("cancelled transaction = %v", err)
	}
	if aws.ToString(tce.CancellationReasons[0].Code) != "ConditionalCheckFailed" || tce.CancellationReasons[0].Item["v"] == nil ||
		aws.ToString(tce.CancellationReasons[1].Code) != "None" {
		t.Errorf("reasons = %+v", tce.CancellationReasons)
	}
	if f.Item(tbl, "c") != nil {
		t.Error("a cancelled transaction applied a write")
	}
	// A write that fails while applying leaves the others unapplied too.
	err = tx(upd("e", "ADD v :one"), types.TransactWriteItem{Update: &types.Update{TableName: aws.String(tbl), Key: key("a"),
		UpdateExpression: aws.String("SET v = nope")}})
	if !isValidation(err) || f.Item(tbl, "e") != nil {
		t.Errorf("transaction with a failing apply = %v; e = %v", err, f.Item(tbl, "e"))
	}
	for name, items := range map[string][]types.TransactWriteItem{
		"empty":         nil,
		"same item":     {upd("a", "ADD v :one"), check("a", "attribute_exists(pk)")},
		"two members":   {{ConditionCheck: check("a", "attribute_exists(pk)").ConditionCheck, Delete: del.Delete}},
		"no condition":  {{ConditionCheck: &types.ConditionCheck{TableName: aws.String(tbl), Key: key("a")}}},
		"no update":     {{Update: &types.Update{TableName: aws.String(tbl), Key: key("a")}}},
		"bad check":     {check("a", "v <>")},
		"bad put":       {{Put: &types.Put{TableName: aws.String(tbl), Item: map[string]av{"x": S("1")}}}},
		"bad update":    {upd("a", "SET")},
		"bad delete":    {{Delete: &types.Delete{TableName: aws.String(tbl), Key: key("")}}},
		"eval error":    {{ConditionCheck: &types.ConditionCheck{TableName: aws.String(tbl), Key: key("a"), ConditionExpression: aws.String("v < :n"), ExpressionAttributeValues: map[string]av{":n": N("x")}}}},
		"missing table": {{Delete: &types.Delete{TableName: aws.String("nope"), Key: key("a")}}},
	} {
		if err := tx(items...); err == nil {
			t.Errorf("%s: transaction succeeded", name)
		}
	}
	for _, ti := range []types.TransactWriteItem{
		{ConditionCheck: &types.ConditionCheck{TableName: aws.String("nope"), Key: key("a"), ConditionExpression: aws.String("attribute_exists(pk)")}},
		{Put: &types.Put{TableName: aws.String("nope"), Item: key("a")}},
		{Update: &types.Update{TableName: aws.String("nope"), Key: key("a"), UpdateExpression: aws.String("REMOVE v")}},
	} {
		if err := tx(ti); !isErr[*types.ResourceNotFoundException](err) {
			t.Errorf("transaction on a missing table = %v", err)
		}
	}
}
