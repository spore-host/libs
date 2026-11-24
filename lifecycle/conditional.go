package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"

	"github.com/spore-host/libs/tags"
)

// Conditional writes, for a teardown that judges an item and must act on it
// only as judged (lagotto#340). Observe and Claim decide on what they read
// themselves: an Observe(false) turns a creator's fresh ACTIVE mark IDLE, and
// a Claim then takes the item at the creator's new epoch, so the claimant
// never sees that a creator arrived. These write only if the item is still
// exactly what the caller read (Expect).
//
// And "in use" with an end. An item is in use while any creator's mark
// (Mark, until its MarkDone) or any hold (AddHold, until RemoveHold) is on
// it: the marks are a set, one per running creator. The last MarkDone or
// RemoveHold, with no mark and no hold left, sets it IDLE, idle from then. A
// scope no backstop observes (the global scope) is therefore ACTIVE exactly
// while something uses it. MarkActive adds the mark "*": activity that no
// creator ends, only a backstop's idle Observe (which, seeing every signal,
// clears every mark).

// ErrChanged is what a conditional write returns when the item is no longer
// what the caller read.
var ErrChanged = errors.New("lifecycle: the item changed since it was read")

// Expect is an item as a caller judged it.
type Expect struct {
	// Exists is false for an item that was absent.
	Exists bool
	Epoch  int64
	State  State
	// LeaseToken is the lease's token, "" for no lease.
	LeaseToken string
	// Marks are the creator marks' tokens, sorted; none for no mark.
	Marks []string
}

// ExpectOf is the Expect of an item read with Get.
func ExpectOf(it Item, exists bool) Expect {
	if !exists {
		return Expect{}
	}
	e := Expect{Exists: true, Epoch: it.Epoch, State: it.State}
	if it.Lease != nil {
		e.LeaseToken = it.Lease.Token
	}
	for _, m := range it.Marks {
		e.Marks = append(e.Marks, m.Token)
	}
	sort.Strings(e.Marks)
	return e
}

// matches reports whether the item read is still e.
func (e Expect) matches(it Item, exists bool) bool {
	x := ExpectOf(it, exists)
	return x.Exists == e.Exists && x.Epoch == e.Epoch && x.State == e.State && x.LeaseToken == e.LeaseToken &&
		strings.Join(x.Marks, "\x00") == strings.Join(e.Marks, "\x00")
}

// cond is e as a condition expression, with its names and values added.
func (e Expect) cond(nm map[string]string, vals map[string]types.AttributeValue) string {
	if !e.Exists {
		nm["#pk"] = attrPK
		return "attribute_not_exists(#pk)"
	}
	nm["#state"], nm["#lease"], nm["#marks"], nm["#epoch"] = attrState, attrLease, attrMarks, attrEpoch
	vals[":xstate"] = s(string(e.State))
	c := epochCond(e.Epoch, vals) + " AND #state = :xstate"
	if e.LeaseToken == "" {
		c += " AND attribute_not_exists(#lease)"
	} else {
		nm["#token"] = attrToken
		vals[":xlease"] = s(e.LeaseToken)
		c += " AND #lease.#token = :xlease"
	}
	if len(e.Marks) == 0 {
		c += " AND attribute_not_exists(#marks)"
	} else {
		vals[":xmarks"] = &types.AttributeValueMemberSS{Value: append([]string(nil), e.Marks...)}
		c += " AND #marks = :xmarks"
	}
	return c
}

// retryable reports whether a write may simply be tried again: a throttle,
// or a transaction cancelled by a conflict rather than a condition.
func retryable(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "ThrottlingException", "ProvisionedThroughputExceededException", "RequestLimitExceeded", "TransactionConflictException":
			return true
		}
	}
	return false
}

// writeAttempts is how many times a conditional write is tried when it is
// throttled or meets a conflicting transaction.
const writeAttempts = 4

// again waits a poll interval before the next attempt.
func (c *Client) again(ctx context.Context) error { return c.o.Sleep(ctx, c.o.PollInterval) }

// Mark is MarkActive, returning the token of the creator mark it added, for
// MarkDone. The item stays ACTIVE until every mark is done and no hold is
// left. The token carries the mark's time (MarkInfo.At).
func (c *Client) Mark(ctx context.Context, tool tags.Tool, scope string) (string, error) {
	token, err := c.newToken()
	if err != nil {
		return "", err
	}
	token = markElement(token, c.now())
	if err := c.activate(ctx, tool, scope, "", token); err != nil {
		return "", err
	}
	return token, nil
}

// MarkDone removes the creator mark token, and, if no mark and no hold is
// left, sets the item IDLE (idleSince now). It reports whether it set the
// item idle: false (with a nil error) while another creator's mark or a hold
// keeps it in use, or when the mark or the item is gone. A throttle or a
// conflicting transaction is tried again.
func (c *Client) MarkDone(ctx context.Context, tool tags.Tool, scope, token string) (bool, error) {
	a, _, key, err := c.target(tool, scope)
	if err != nil {
		return false, err
	}
	for attempt := 1; ; attempt++ {
		_, err = a.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                 aws.String(TableName),
			Key:                       keyOf(key),
			UpdateExpression:          aws.String("DELETE #marks :mark"),
			ConditionExpression:       aws.String("contains(#marks, :one)"),
			ExpressionAttributeNames:  names(attrMarks),
			ExpressionAttributeValues: map[string]types.AttributeValue{":mark": ss(token), ":one": s(token)},
		})
		if err == nil || isCCF(err) || isNotFound(err) || !retryable(err) || attempt == writeAttempts {
			break
		}
		if err := c.again(ctx); err != nil {
			return false, err
		}
	}
	switch {
	case isCCF(err), isNotFound(err):
		return false, nil // not on it (already done, or cleared by a backstop)
	case err != nil:
		return false, fmt.Errorf("lifecycle: end mark on %s: %w", key, err)
	}
	return c.idleIfFree(ctx, a, key)
}

// idleIfFree sets the item IDLE (idleSince now) if it is ACTIVE with no
// mark, no hold and no live lease, in one transaction that checks the closing
// sentinel. It reports whether it did. A throttle or conflict is tried again.
func (c *Client) idleIfFree(ctx context.Context, a API, key string) (bool, error) {
	for attempt := 1; ; attempt++ {
		now := c.now()
		nm := names(attrState, attrIdleSince, attrMarks, attrHolds, attrLease, attrHeartbeat)
		vals := map[string]types.AttributeValue{
			":idle": s(string(StateIdle)), ":active": s(string(StateActive)),
			":now": s(formatTime(now)), ":stale": s(c.staleBefore(now)),
		}
		_, err := a.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
			sentinelAbsent(),
			{Update: &types.Update{
				TableName:        aws.String(TableName),
				Key:              keyOf(key),
				UpdateExpression: aws.String("SET #state = :idle, #idleSince = :now"),
				ConditionExpression: aws.String("#state = :active AND attribute_not_exists(#marks) AND attribute_not_exists(#holds) AND " +
					noLiveLease),
				ExpressionAttributeNames:            nm,
				ExpressionAttributeValues:           vals,
				ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
			}},
		}})
		if err == nil {
			return true, nil
		}
		if isNotFound(err) {
			return false, nil
		}
		b, cerr := classifyCancel(err)
		switch {
		case cerr != nil && (!retryable(cerr) || attempt == writeAttempts):
			return false, fmt.Errorf("lifecycle: set %s idle: %w", key, cerr)
		case cerr != nil:
		case b.what == "sentinel":
			return false, ErrTableClosing
		case b.what == "lease":
			return false, nil // still in use, or claimed
		case attempt == writeAttempts:
			return false, fmt.Errorf("lifecycle: set %s idle: transaction conflict", key)
		}
		if err := c.again(ctx); err != nil {
			return false, err
		}
	}
}

// SetIdleIfUnchanged records tool idle in scope, as the caller judged it: if
// the item is still exactly e it becomes IDLE (idleSince now, if not set),
// with no mark (the marks judged were, by the caller's own rule, creators
// that died) and no lease (one judged stale; never a live one). An absent
// item (e.Exists false) is created IDLE, and the table with it. Otherwise it
// writes nothing and returns ErrChanged. No hold may be on it. It checks the
// closing sentinel in the same transaction (ErrTableClosing). A throttle or
// conflict is tried again.
func (c *Client) SetIdleIfUnchanged(ctx context.Context, tool tags.Tool, scope string, e Expect) (Item, error) {
	a, region, key, err := c.target(tool, scope)
	if err != nil {
		return Item{}, err
	}
	start := c.now()
	missing := 0
	for attempt := 1; ; attempt++ {
		now := c.now()
		nm := names(attrIdleSince, attrTool, attrScope, attrHolds, attrHeartbeat)
		nm["#state"], nm["#marks"], nm["#lease"] = attrState, attrMarks, attrLease
		vals := map[string]types.AttributeValue{":idle": s(string(StateIdle)), ":now": s(formatTime(now)),
			":tool": s(string(tool)), ":scope": s(scope), ":stale": s(c.staleBefore(now))}
		cond := e.cond(nm, vals) + " AND attribute_not_exists(#holds) AND " + noLiveLease
		_, err := a.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
			sentinelAbsent(),
			{Update: &types.Update{
				TableName: aws.String(TableName),
				Key:       keyOf(key),
				UpdateExpression: aws.String("SET #state = :idle, #idleSince = if_not_exists(#idleSince, :now), #tool = :tool, " +
					"#scope = :scope REMOVE #marks, #lease"),
				ConditionExpression:                 aws.String(cond),
				ExpressionAttributeNames:            nm,
				ExpressionAttributeValues:           vals,
				ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
			}},
		}})
		if err == nil {
			it, _, err := c.get(ctx, a, key)
			return it, err
		}
		if isNotFound(err) {
			if e.Exists {
				return Item{}, fmt.Errorf("lifecycle: set %s idle: the table is gone: %w", key, ErrChanged)
			}
			if missing++; missing > 2 {
				return Item{}, fmt.Errorf("lifecycle: set %s idle: no table: %w", key, err)
			}
			if err := c.ensureTable(ctx, a, region, tool, scope, start); err != nil {
				return Item{}, err
			}
			continue
		}
		b, cerr := classifyCancel(err)
		switch {
		case cerr != nil && (!retryable(cerr) || attempt == writeAttempts):
			return Item{}, fmt.Errorf("lifecycle: set %s idle: %w", key, cerr)
		case cerr != nil:
		case b.what == "sentinel":
			return Item{}, ErrTableClosing
		case b.what == "lease":
			return Item{}, fmt.Errorf("lifecycle: set %s idle: %w", key, ErrChanged)
		case attempt == writeAttempts:
			return Item{}, fmt.Errorf("lifecycle: set %s idle: transaction conflict", key)
		}
		if err := c.again(ctx); err != nil {
			return Item{}, err
		}
	}
}

// ClaimIfUnchanged takes the teardown lease as Claim does, but only if the
// item is still exactly e: an IDLE item idle for at least grace, or a
// TEARDOWN one whose lease (e.LeaseToken) has gone stale, which it takes
// over. No hold may exist. Any difference (a creator marked, a hold, another
// claim) refuses it with a *NotClaimableError ("changed since it was
// judged").
func (c *Client) ClaimIfUnchanged(ctx context.Context, tool tags.Tool, scope string, grace time.Duration, e Expect) (*Lease, error) {
	if grace < 0 {
		return nil, errors.New("lifecycle: negative grace")
	}
	a, _, key, err := c.target(tool, scope)
	if err != nil {
		return nil, err
	}
	refuse := func(why string) error { return &NotClaimableError{Tool: tool, Scope: scope, Reason: why} }
	it, exists, err := c.get(ctx, a, key)
	switch {
	case isNotFound(err):
		return nil, refuse("no lifecycle table")
	case err != nil:
		return nil, fmt.Errorf("lifecycle: read %s: %w", key, err)
	case !exists:
		return nil, refuse("no lifecycle item")
	case !e.matches(it, exists):
		return nil, refuse("changed since it was judged")
	case len(it.Holds) > 0:
		return nil, refuse(fmt.Sprintf("held by %v", it.Holds))
	}
	now := c.now()
	switch {
	case it.State == StateTeardown && it.Lease != nil && c.isStale(it.Lease.Heartbeat, now):
	case it.State == StateIdle && it.Lease == nil && !it.IdleSince.IsZero():
		if it.IdleFor(now) < grace {
			return nil, refuse(fmt.Sprintf("idle for %s of %s", it.IdleFor(now).Round(time.Second), grace))
		}
	case it.Lease != nil && !c.isStale(it.Lease.Heartbeat, now):
		return nil, refuse("lease held by " + it.Lease.Owner)
	default:
		return nil, refuse("state is " + string(it.State))
	}
	token, err := c.newToken()
	if err != nil {
		return nil, err
	}
	nm := names(attrHolds, attrHeartbeat, attrIncomplete)
	vals := map[string]types.AttributeValue{
		":teardown": s(string(StateTeardown)),
		":stale":    s(c.staleBefore(now)),
		":lease": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
			attrToken: s(token), attrOwner: s(c.o.Owner), attrHeartbeat: s(formatTime(now)), attrStep: s(""),
		}},
	}
	cond := e.cond(nm, vals) + " AND attribute_not_exists(#holds) AND " + noLiveLease
	if e.State == StateIdle {
		nm["#idleSince"] = attrIdleSince
		vals[":cutoff"] = s(formatTime(now.Add(-grace)))
		cond += " AND #idleSince <= :cutoff"
	}
	_, err = a.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(TableName),
		Key:                       keyOf(key),
		UpdateExpression:          aws.String("SET #state = :teardown, #lease = :lease REMOVE #incomplete"),
		ConditionExpression:       aws.String(cond),
		ExpressionAttributeNames:  nm,
		ExpressionAttributeValues: vals,
	})
	switch {
	case isCCF(err):
		return nil, refuse("changed while claiming")
	case isNotFound(err):
		return nil, refuse("no lifecycle table")
	case err != nil:
		return nil, fmt.Errorf("lifecycle: claim %s: %w", key, err)
	}
	return &Lease{c: c, a: a, key: key, tool: tool, scope: scope, token: token}, nil
}

// LockIfUnchanged takes a lease on the item without changing its state, if
// the item is still exactly e and no live lease is on it: a short exclusion,
// for maintenance a backstop must not race (lagotto#340: deleting a region's
// pending teardown jobs). While it is held, Claim refuses, an idle Observe
// records nothing, and MarkActive waits; an active Observe, or a creator
// taking it over once stale, ends it (Step then fails). Unlock gives it back.
func (c *Client) LockIfUnchanged(ctx context.Context, tool tags.Tool, scope string, e Expect) (*Lease, error) {
	a, _, key, err := c.target(tool, scope)
	if err != nil {
		return nil, err
	}
	if !e.Exists {
		return nil, &NotClaimableError{Tool: tool, Scope: scope, Reason: "no lifecycle item"}
	}
	token, err := c.newToken()
	if err != nil {
		return nil, err
	}
	now := c.now()
	nm := names(attrHeartbeat)
	vals := map[string]types.AttributeValue{
		":stale": s(c.staleBefore(now)),
		":lease": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
			attrToken: s(token), attrOwner: s(c.o.Owner), attrHeartbeat: s(formatTime(now)), attrStep: s("lock"),
		}},
	}
	cond := e.cond(nm, vals) + " AND " + noLiveLease
	_, err = a.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(TableName),
		Key:                       keyOf(key),
		UpdateExpression:          aws.String("SET #lease = :lease"),
		ConditionExpression:       aws.String(cond),
		ExpressionAttributeNames:  nm,
		ExpressionAttributeValues: vals,
	})
	switch {
	case isCCF(err), isNotFound(err):
		return nil, &NotClaimableError{Tool: tool, Scope: scope, Reason: "changed since it was judged"}
	case err != nil:
		return nil, fmt.Errorf("lifecycle: lock %s: %w", key, err)
	}
	return &Lease{c: c, a: a, key: key, tool: tool, scope: scope, token: token}, nil
}

// Unlock gives back a lease LockIfUnchanged took, leaving the item's state
// as it was.
func (l *Lease) Unlock(ctx context.Context) error {
	return l.write(ctx, "unlock", func(cond string, nm map[string]string, vals map[string]types.AttributeValue) error {
		_, err := l.a.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                 aws.String(TableName),
			Key:                       keyOf(l.key),
			UpdateExpression:          aws.String("REMOVE #lease"),
			ConditionExpression:       aws.String(cond),
			ExpressionAttributeNames:  nm,
			ExpressionAttributeValues: vals,
		})
		return err
	}, true)
}

// AdoptLease returns the live lease on tool in scope held in this Client's
// owner name: a claim whose answer was lost (a dropped connection after the
// write applied) leaves one the caller cannot otherwise end. Give it back
// with Release, Suspend or Unlock. ErrLeaseLost when there is no such lease.
// Owner names must be unique to a run for this to be safe.
func (c *Client) AdoptLease(ctx context.Context, tool tags.Tool, scope string) (*Lease, error) {
	a, _, key, err := c.target(tool, scope)
	if err != nil {
		return nil, err
	}
	it, exists, err := c.get(ctx, a, key)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: read %s: %w", key, err)
	}
	if !exists || it.Lease == nil || it.Lease.Owner != c.o.Owner || c.isStale(it.Lease.Heartbeat, c.now()) {
		return nil, fmt.Errorf("lifecycle: no live lease of %s on %s: %w", c.o.Owner, key, ErrLeaseLost)
	}
	return &Lease{c: c, a: a, key: key, tool: tool, scope: scope, token: it.Lease.Token}, nil
}

// Suspend gives the lease up after a teardown stopped partway: the item goes
// IDLE (keeping its idleSince), marked incomplete, with no lease, so no
// creator waits for it, and the next backstop resumes the teardown (it reads
// Item.Incomplete). Any activity (MarkActive, Mark, AddHold, an active
// Observe), a Claim and a Release clear the mark.
func (l *Lease) Suspend(ctx context.Context) error {
	now := l.c.now()
	return l.write(ctx, "suspend", func(cond string, nm map[string]string, vals map[string]types.AttributeValue) error {
		nm["#state"], nm["#idleSince"], nm["#incomplete"] = attrState, attrIdleSince, attrIncomplete
		vals[":idle"], vals[":now"], vals[":true"] = s(string(StateIdle)), s(formatTime(now)), &types.AttributeValueMemberBOOL{Value: true}
		_, err := l.a.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                 aws.String(TableName),
			Key:                       keyOf(l.key),
			UpdateExpression:          aws.String("SET #state = :idle, #idleSince = if_not_exists(#idleSince, :now), #incomplete = :true REMOVE #lease"),
			ConditionExpression:       aws.String(cond),
			ExpressionAttributeNames:  nm,
			ExpressionAttributeValues: vals,
		})
		return err
	}, true)
}
