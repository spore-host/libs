package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/spore-host/libs/tags"
)

// Conditional writes, for a teardown that judges an item and must act on it
// only as judged (lagotto#340). Observe and Claim decide on what they read
// themselves: an Observe(false) turns a creator's fresh ACTIVE mark IDLE, and
// a Claim then takes the item at the creator's new epoch, so the claimant
// never sees that a creator arrived. These write only if the item is still
// exactly what the caller read (Expect).
//
// And creator marks with an end: Mark returns a token, and MarkDone ends
// that mark (the item goes IDLE) unless another creator has marked since. A
// scope no backstop observes (the global scope) is ACTIVE exactly while a
// creator is running, then idle from the moment the last one finished.

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
	// MarkToken is the creator mark's token, "" for no mark.
	MarkToken string
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
	if it.Mark != nil {
		e.MarkToken = it.Mark.Token
	}
	return e
}

// matches reports whether the item read is still e.
func (e Expect) matches(it Item, exists bool) bool {
	return ExpectOf(it, exists) == e
}

// cond is e as a condition expression, with its names and values added.
func (e Expect) cond(nm map[string]string, vals map[string]types.AttributeValue) string {
	if !e.Exists {
		nm["#pk"] = attrPK
		return "attribute_not_exists(#pk)"
	}
	nm["#state"], nm["#lease"], nm["#mark"] = attrState, attrLease, attrMark
	vals[":xstate"] = s(string(e.State))
	c := epochCond(e.Epoch, vals) + " AND #state = :xstate"
	nm["#epoch"] = attrEpoch
	if e.LeaseToken == "" {
		c += " AND attribute_not_exists(#lease)"
	} else {
		nm["#token"] = attrToken
		vals[":xlease"] = s(e.LeaseToken)
		c += " AND #lease.#token = :xlease"
	}
	if e.MarkToken == "" {
		c += " AND attribute_not_exists(#mark)"
	} else {
		nm["#token"] = attrToken
		vals[":xmark"] = s(e.MarkToken)
		c += " AND #mark.#token = :xmark"
	}
	return c
}

// Mark is MarkActive, returning the token of the creator mark it wrote, for
// MarkDone. The mark (its token and time) stays on the item while it is
// ACTIVE from it; the next Mark replaces it.
func (c *Client) Mark(ctx context.Context, tool tags.Tool, scope string) (string, error) {
	token, err := c.newToken()
	if err != nil {
		return "", err
	}
	if err := c.activate(ctx, tool, scope, "", token); err != nil {
		return "", err
	}
	return token, nil
}

// MarkDone ends the creator mark token: the item goes IDLE (idleSince now),
// in one transaction that checks the closing sentinel, on condition that it
// is still ACTIVE from that same mark, with no lease and no hold. It reports
// whether it did: false (with a nil error) when another creator has marked
// since (that one ends it), a hold keeps the tool active, or the item is
// gone.
func (c *Client) MarkDone(ctx context.Context, tool tags.Tool, scope, token string) (bool, error) {
	a, _, key, err := c.target(tool, scope)
	if err != nil {
		return false, err
	}
	nm := names(attrState, attrIdleSince, attrMark, attrToken, attrLease, attrHolds)
	vals := map[string]types.AttributeValue{
		":idle": s(string(StateIdle)), ":active": s(string(StateActive)),
		":now": s(formatTime(c.now())), ":mark": s(token),
	}
	_, err = a.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		sentinelAbsent(),
		{Update: &types.Update{
			TableName:        aws.String(TableName),
			Key:              keyOf(key),
			UpdateExpression: aws.String("SET #state = :idle, #idleSince = :now REMOVE #mark"),
			ConditionExpression: aws.String("#state = :active AND #mark.#token = :mark AND attribute_not_exists(#lease) " +
				"AND attribute_not_exists(#holds)"),
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
	b, err := classifyCancel(err)
	switch {
	case err != nil:
		return false, fmt.Errorf("lifecycle: end mark on %s: %w", key, err)
	case b.what == "sentinel":
		return false, ErrTableClosing
	case b.what == "lease":
		return false, nil // marked again since, held, or claimed
	}
	return false, fmt.Errorf("lifecycle: end mark on %s: transaction conflict", key)
}

// SetIdleIfUnchanged records tool idle in scope, as the caller judged it: if
// the item is still exactly e it becomes IDLE (idleSince now, if not set),
// with no mark; an absent item (e.Exists false) is created IDLE, and the
// table with it. Otherwise it writes nothing and returns ErrChanged. It
// checks the closing sentinel in the same transaction (ErrTableClosing). A
// lease in e (one gone stale) is removed with it.
func (c *Client) SetIdleIfUnchanged(ctx context.Context, tool tags.Tool, scope string, e Expect) (Item, error) {
	a, region, key, err := c.target(tool, scope)
	if err != nil {
		return Item{}, err
	}
	start := c.now()
	for attempt := 1; ; attempt++ {
		nm := names(attrIdleSince, attrTool, attrScope)
		nm["#state"], nm["#mark"], nm["#lease"] = attrState, attrMark, attrLease
		vals := map[string]types.AttributeValue{":idle": s(string(StateIdle)), ":now": s(formatTime(c.now())),
			":tool": s(string(tool)), ":scope": s(scope)}
		cond := e.cond(nm, vals) + " AND attribute_not_exists(#holds)"
		nm["#holds"] = attrHolds
		_, err := a.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
			sentinelAbsent(),
			{Update: &types.Update{
				TableName: aws.String(TableName),
				Key:       keyOf(key),
				UpdateExpression: aws.String("SET #state = :idle, #idleSince = if_not_exists(#idleSince, :now), #tool = :tool, " +
					"#scope = :scope REMOVE #mark, #lease"),
				ConditionExpression:                 aws.String(cond),
				ExpressionAttributeNames:            nm,
				ExpressionAttributeValues:           vals,
				ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
			}},
		}})
		if isNotFound(err) && !e.Exists && attempt < 3 {
			if err := c.ensureTable(ctx, a, region, tool, scope, start); err != nil {
				return Item{}, err
			}
			continue
		}
		if err == nil {
			it, _, err := c.get(ctx, a, key)
			return it, err
		}
		if isNotFound(err) {
			return Item{}, fmt.Errorf("lifecycle: set %s idle: %w", key, ErrChanged)
		}
		b, cerr := classifyCancel(err)
		switch {
		case cerr != nil:
			return Item{}, fmt.Errorf("lifecycle: set %s idle: %w", key, cerr)
		case b.what == "sentinel":
			return Item{}, ErrTableClosing
		case b.what == "lease":
			return Item{}, fmt.Errorf("lifecycle: set %s idle: %w", key, ErrChanged)
		}
		return Item{}, fmt.Errorf("lifecycle: set %s idle: transaction conflict", key)
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
	case !exists || !e.Exists:
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

// Suspend gives the lease up after a teardown stopped partway: the item goes
// IDLE (keeping its idleSince), marked incomplete, with no lease, so no
// creator waits for it, and the next backstop resumes the teardown (it reads
// Item.Incomplete). Any activity (MarkActive, Mark, an active Observe)
// clears the mark: the tool is in use again.
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
