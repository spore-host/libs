package lifecycle

import (
	"context"
	"errors"
	"fmt"
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
// And "in use" a reader can judge. Writers only add or remove what keeps an
// item in use (a creator's mark, Mark until its MarkDone; a hold, AddHold
// until RemoveHold; a pulse, MarkActive) and stamp when they did (and an
// active Observe or a Release stamps the time too); none of them ever sets an item IDLE. Whether an
// item is in use, and since when it has been idle, is the reader's to judge,
// from those times (InUse, IdleSince): an item a writer could not finish
// with ages out by itself.

// ErrChanged is what a conditional write returns when the item is no longer
// what the caller read.
var ErrChanged = errors.New("lifecycle: the item changed since it was read")

// ErrApplied wraps the error of a read after a conditional write that was
// applied: the write stands; only its result could not be read.
var ErrApplied = errors.New("lifecycle: the write was applied")

// Expect is an item as a caller judged it.
type Expect struct {
	// Exists is false for an item that was absent.
	Exists bool
	Epoch  int64
	State  State
	// LeaseToken is the lease's token, "" for no lease.
	LeaseToken string
	// LastActivity is the item's LastActivityAt, the one change detector:
	// every write that changes what keeps the item in use (a mark made,
	// refreshed or ended, a hold, MarkActive, an active Observe, a Release)
	// stamps it.
	LastActivity time.Time
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
	e.LastActivity = it.LastActivityAt
	return e
}

// matches reports whether the item read is still e.
func (e Expect) matches(it Item, exists bool) bool {
	x := ExpectOf(it, exists)
	return x.Exists == e.Exists && x.Epoch == e.Epoch && x.State == e.State && x.LeaseToken == e.LeaseToken &&
		x.LastActivity.Equal(e.LastActivity)
}

// cond is e as a condition expression, with its names and values added.
func (e Expect) cond(nm map[string]string, vals map[string]types.AttributeValue) string {
	if !e.Exists {
		nm["#pk"] = attrPK
		return "attribute_not_exists(#pk)"
	}
	nm["#state"], nm["#lease"], nm["#epoch"] = attrState, attrLease, attrEpoch
	vals[":xstate"] = s(string(e.State))
	c := epochCond(e.Epoch, vals) + " AND #state = :xstate"
	if e.LeaseToken == "" {
		c += " AND attribute_not_exists(#lease)"
	} else {
		nm["#token"] = attrToken
		vals[":xlease"] = s(e.LeaseToken)
		c += " AND #lease.#token = :xlease"
	}
	return c + " AND " + laEqual(e.LastActivity, nm, vals)
}

// laEqual is the condition that lastActivityAt is still t (absent for a
// zero t): the one change detector, since every write that changes what
// keeps an item in use stamps it with its writer's clock. Writers' clocks
// are assumed to agree within a few minutes (NTP on Lambda and CLI hosts);
// grace and mark bounds are tens of minutes or more, so the skew is
// immaterial, and two writes stamping the same millisecond is the one change
// this cannot see.
func laEqual(t time.Time, nm map[string]string, vals map[string]types.AttributeValue) string {
	nm["#lastActivityAt"] = attrLastActive
	if t.IsZero() {
		return "attribute_not_exists(#lastActivityAt)"
	}
	vals[":xlast"] = s(formatTime(t))
	return "#lastActivityAt = :xlast"
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

// Mark records that a creator is running in scope: it sets the creator's
// mark (marks[token] = the time it commits), as MarkActive activates it, and
// returns the token for MarkDone. A mark keeps the item in use (InUse) until
// MarkDone removes it, or until it is older than the reader's mark bound (a
// creator that died). A creator that may run longer than the bound its
// readers use (Options.MarkBound for an idle Observe) must refresh its mark
// (RefreshMark) well within it, at a third of it say. On an error the token
// is returned too: a mark that was committed though its answer was lost can
// be ended with it, and ages out if it is not.
func (c *Client) Mark(ctx context.Context, tool tags.Tool, scope string) (string, error) {
	token, err := c.newToken()
	if err != nil {
		return "", err
	}
	return token, c.activate(ctx, tool, scope, "", token)
}

// MarkDone removes the creator mark token, and stamps the item's
// LastActivityAt. It never changes the item's state: whether the item is
// still in use, and since when it is idle, is the reader's to judge
// (InUse, IdleSince). It reports whether it removed the mark: false (with a
// nil error) when the mark or the item is gone already. One write, no
// read; a throttle is tried again.
func (c *Client) MarkDone(ctx context.Context, tool tags.Tool, scope, token string) (bool, error) {
	return c.markWrite(ctx, tool, scope, token, "REMOVE #marks.#tok", "", "end mark")
}

// noLiveClaim is noLiveLease, but a lock (LockIfUnchanged) does not count.
const noLiveClaim = "(" + noLiveLease + " OR #lease.#kind = :lock)"

// RefreshMark re-stamps the creator mark token with the time now, only if it
// is still on the item and no teardown's claim holds the item's live lease
// (a backstop's short lock does not stop it): a
// creator that runs longer than its readers' mark bound calls it well within
// the bound, so its mark is never taken for a creator that died. It reports
// whether it refreshed the mark; false (with a nil error) means the mark is
// gone (aged out and removed, or ended), or a teardown holds the item: the
// creator should stop creating. One write, no read.
func (c *Client) RefreshMark(ctx context.Context, tool tags.Tool, scope, token string) (bool, error) {
	return c.markWrite(ctx, tool, scope, token, ", #marks.#tok = :now", " AND "+noLiveClaim, "refresh mark")
}

// markWrite is MarkDone's and RefreshMark's one write on token's mark,
// stamping lastActivityAt, on condition that the mark is there (and cond).
// A retry whose earlier try may have applied (a throttle or conflict can
// follow a write that went through) judges its CCF by the item as it is: a
// done whose mark is gone, or a refresh whose mark has this very time, was
// applied.
func (c *Client) markWrite(ctx context.Context, tool tags.Tool, scope, token, upd, cond, what string) (bool, error) {
	if token == "" {
		return false, nil // no mark was written
	}
	a, _, key, err := c.target(tool, scope)
	if err != nil {
		return false, err
	}
	now := c.now()
	nm := names(attrMarks, attrLastActive)
	nm["#tok"] = token
	vals := map[string]types.AttributeValue{":now": s(formatTime(now))}
	if cond != "" {
		nm["#lease"], nm["#heartbeat"], nm["#kind"] = attrLease, attrHeartbeat, attrKind
		vals[":stale"], vals[":lock"] = s(c.staleBefore(now)), s(leaseKindLock)
	}
	expr := "SET #lastActivityAt = :now" + upd
	if upd[0] != ',' {
		expr = "SET #lastActivityAt = :now " + upd
	}
	retried := false
	for attempt := 1; ; attempt++ {
		_, err = a.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                           aws.String(TableName),
			Key:                                 keyOf(key),
			UpdateExpression:                    aws.String(expr),
			ConditionExpression:                 aws.String("attribute_exists(#marks.#tok)" + cond),
			ExpressionAttributeNames:            nm,
			ExpressionAttributeValues:           vals,
			ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
		})
		if err == nil || !retryable(err) || attempt >= writeAttempts {
			break
		}
		retried = true
		if err := c.again(ctx); err != nil {
			return false, err
		}
	}
	// Times compare as stored: now is at the stored precision already.
	var cc *types.ConditionalCheckFailedException
	if retried && errors.As(err, &cc) && len(cc.Item) > 0 {
		old := decodeItem(cc.Item)
		at, has := "", false
		for _, m := range old.Marks {
			if m.Token == token {
				at, has = formatTime(m.At), true
			}
		}
		ours := formatTime(old.LastActivityAt) == formatTime(now) // this call's stamp
		if (cond == "" && !has && ours) || (cond != "" && has && at == formatTime(now)) {
			return true, nil // an earlier try applied
		}
	}
	switch {
	case isCCF(err), isNotFound(err):
		return false, nil // not on it (gone, ended, or a teardown holds it)
	case err != nil:
		return false, fmt.Errorf("lifecycle: %s on %s: %w", what, key, err)
	}
	return true, nil
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
	missing, cleared := 0, 0
	madeTable := false // the last attempt made the table (ensureTable waited for it)
	for attempt := 1; ; attempt++ {
		if attempt > 1 {
			// Every retry counts, none goes past writeAttempts, and each
			// backs off but the one after making the table.
			if attempt > writeAttempts {
				return Item{}, fmt.Errorf("lifecycle: set %s idle: gave up after %d attempts: %w", key, writeAttempts, err)
			}
			if !madeTable {
				if err := c.again(ctx); err != nil {
					return Item{}, err
				}
			}
		}
		madeTable = false
		now := c.now()
		nm := names(attrIdleSince, attrTool, attrScope, attrHolds, attrHeartbeat)
		nm["#state"], nm["#marks"], nm["#lease"] = attrState, attrMarks, attrLease
		vals := map[string]types.AttributeValue{":idle": s(string(StateIdle)), ":now": s(formatTime(now)),
			":tool": s(string(tool)), ":scope": s(scope), ":stale": s(c.staleBefore(now))}
		cond := e.cond(nm, vals) + " AND attribute_not_exists(#holds) AND " + noLiveLease
		upStamp := stamp(StateIdle, false, nm, vals)
		_, err = a.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
			sentinelAbsent(),
			{Update: &types.Update{
				TableName: aws.String(TableName),
				Key:       keyOf(key),
				UpdateExpression: aws.String("SET #state = :idle, #idleSince = if_not_exists(#idleSince, :now), #tool = :tool, " +
					"#scope = :scope" + upStamp + " REMOVE #marks, #lease"),
				ConditionExpression:                 aws.String(cond),
				ExpressionAttributeNames:            nm,
				ExpressionAttributeValues:           vals,
				ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
			}},
		}})
		if err == nil {
			it, _, err := c.get(ctx, a, key)
			if err != nil {
				return Item{}, fmt.Errorf("lifecycle: set %s idle: %w; reading it back: %w", key, ErrApplied, err)
			}
			return it, nil
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
			madeTable = true
			continue
		}
		b, cerr := classifyCancel(err)
		switch {
		case cerr != nil && !retryable(cerr):
			return Item{}, fmt.Errorf("lifecycle: set %s idle: %w", key, cerr)
		case cerr != nil:
			err = cerr
		case b.what == "sentinel" && !b.sentinel.IsZero() && c.isStale(b.sentinel, c.now()) && cleared < 2:
			// A closer that died left it (as activate clears one).
			cleared++
			if err := c.clearStaleSentinel(ctx, a, c.now()); err != nil {
				return Item{}, err
			}
			err = ErrTableClosing
		case b.what == "sentinel":
			return Item{}, ErrTableClosing
		case b.what == "lease":
			return Item{}, fmt.Errorf("lifecycle: set %s idle: %w", key, changedOrInUse(e, b.item))
		default:
			err = errors.New("transaction conflict")
		}
	}
}

// ErrInUse is a conditional idle's refusal of an item that is still as the
// caller judged it, but has a hold or a live lease on it: in use, not
// changed.
var ErrInUse = errors.New("lifecycle: the item is in use (a hold or a live lease)")

// changedOrInUse is why a conditional idle of e was refused, from the item
// the refusal returned: ErrInUse when it is still e but held or leased,
// ErrChanged otherwise.
func changedOrInUse(e Expect, old map[string]types.AttributeValue) error {
	it := decodeItem(old)
	if old != nil && e.matches(it, true) && (len(it.Holds) > 0 || it.Lease != nil) {
		return ErrInUse
	}
	return ErrChanged
}

// ClaimIfUnchanged takes the teardown lease as Claim does, but only if the
// item is still exactly e: an IDLE item idle for at least grace (with no
// lease, or a stale one, which it takes over), or a TEARDOWN one whose lease
// (e.LeaseToken) has gone stale, which it takes over. A caller that judged
// the item with IdleSince passes grace 0. No hold may exist. Any difference (a creator marked, a hold, another
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
	case liveMark(it, c.now(), c.o.MarkBound):
		// A creator still running: e pins lastActivityAt, so this read
		// (which matches it) decides.
		return nil, refuse("a creator's mark is live")
	case len(it.Holds) > 0:
		return nil, refuse(fmt.Sprintf("held by %v", it.Holds))
	}
	now := c.now()
	switch {
	case it.State == StateTeardown && it.Lease != nil && c.leaseStale(it.Lease, now):
	case it.State == StateIdle && (it.Lease == nil || c.leaseStale(it.Lease, now)) && !it.IdleSince.IsZero():
		// IDLE, with no lease or one whose holder died (a lock): the grace
		// is the item's, whoever held a lease on it.
		if it.IdleFor(now) < grace {
			return nil, refuse(fmt.Sprintf("idle for %s of %s", it.IdleFor(now).Round(time.Second), grace))
		}
	case it.Lease != nil && !c.leaseStale(it.Lease, now):
		return nil, refuse("lease held by " + it.Lease.Owner)
	default:
		return nil, refuse("state is " + string(it.State))
	}
	token, err := c.newToken()
	if err != nil {
		return nil, err
	}
	id, err := c.clientID()
	if err != nil {
		return nil, err
	}
	nm := names(attrHolds, attrHeartbeat, attrIncomplete)
	vals := map[string]types.AttributeValue{
		":teardown": s(string(StateTeardown)),
		":stale":    s(c.staleBefore(now)),
		":lease": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
			attrToken: s(token), attrOwner: s(c.o.Owner), attrClient: s(id), attrHeartbeat: s(formatTime(now)), attrStep: s(""),
		}},
	}
	cond := e.cond(nm, vals) + " AND attribute_not_exists(#holds) AND " + noLiveLease
	if e.State == StateIdle && grace > 0 {
		nm["#idleSince"] = attrIdleSince
		vals[":cutoff"] = s(formatTime(now.Add(-grace)))
		cond += " AND #idleSince <= :cutoff"
	}
	_, err = a.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(TableName),
		Key:                       keyOf(key),
		UpdateExpression:          aws.String("SET #state = :teardown, #lease = :lease" + stamp(StateTeardown, false, nm, vals) + " REMOVE #incomplete"),
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
// the item is still exactly e, is not TEARDOWN, and no live lease is on it: a short exclusion,
// for maintenance a backstop must not race (lagotto#340: deleting a region's
// pending teardown jobs). While it is held, Claim refuses, an idle Observe
// records nothing, and MarkActive waits; an active Observe, or a creator
// taking it over once stale, ends it (Step then fails). Unlock gives it back.
func (c *Client) LockIfUnchanged(ctx context.Context, tool tags.Tool, scope string, e Expect) (*Lease, error) {
	a, _, key, err := c.target(tool, scope)
	if err != nil {
		return nil, err
	}
	switch {
	case !e.Exists:
		return nil, &NotClaimableError{Tool: tool, Scope: scope, Reason: "no lifecycle item"}
	case e.State == StateTeardown:
		// A teardown's item, its lease live or not: a lock taken over it
		// would leave it TEARDOWN with no lease once unlocked.
		return nil, &NotClaimableError{Tool: tool, Scope: scope, Reason: "state is TEARDOWN"}
	}
	token, err := c.newToken()
	if err != nil {
		return nil, err
	}
	id, err := c.clientID()
	if err != nil {
		return nil, err
	}
	now := c.now()
	nm := names(attrHeartbeat)
	vals := map[string]types.AttributeValue{
		":stale": s(c.staleBefore(now)),
		":lease": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
			attrToken: s(token), attrOwner: s(c.o.Owner), attrClient: s(id), attrHeartbeat: s(formatTime(now)), attrStep: s("lock"), attrKind: s(leaseKindLock),
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
	return &Lease{c: c, a: a, key: key, tool: tool, scope: scope, token: token, lock: true}, nil
}

// ErrNotALock is the refusal of a lease of the wrong kind: Unlock of a
// teardown claim (it would leave the item TEARDOWN with no lease and nothing
// to say the teardown is incomplete: give a claim back with Release or
// Suspend), and Release or Suspend of a lock (give it back with Unlock).
var ErrNotALock = errors.New("lifecycle: the lease is of the other kind: a claim goes back with Release or Suspend, a lock with Unlock")

// Unlock gives back a lease LockIfUnchanged took, leaving the item's state
// as it was. A claim's lease (Claim, ClaimIfUnchanged, or one AdoptLease
// found) it refuses with ErrNotALock.
func (l *Lease) Unlock(ctx context.Context) error {
	if !l.lock {
		return fmt.Errorf("lifecycle: unlock %s: %w", l.key, ErrNotALock)
	}
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

// AdoptLease returns the live lease on tool in scope that this very Client
// took (its owner name and its own random id both match): a claim or lock
// whose answer was lost (a dropped connection after the write applied)
// leaves one the caller cannot otherwise end. Give a claim back with Release
// or Suspend, and a lock with Unlock (which refuses a claim: ErrNotALock).
// ErrLeaseLost when there is no such lease. A Client with no owner set
// ("unknown") adopts nothing.
func (c *Client) AdoptLease(ctx context.Context, tool tags.Tool, scope string) (*Lease, error) {
	a, _, key, err := c.target(tool, scope)
	if err != nil {
		return nil, err
	}
	it, exists, err := c.get(ctx, a, key)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: read %s: %w", key, err)
	}
	if c.o.Owner == "" || c.o.Owner == "unknown" {
		return nil, fmt.Errorf("lifecycle: a client with no owner adopts no lease: %w", ErrLeaseLost)
	}
	if !exists || it.Lease == nil || it.Lease.Owner != c.o.Owner || it.Lease.Client != c.adoptID() || c.leaseStale(it.Lease, c.now()) {
		return nil, fmt.Errorf("lifecycle: no live lease of %s on %s: %w", c.o.Owner, key, ErrLeaseLost)
	}
	return &Lease{c: c, a: a, key: key, tool: tool, scope: scope, token: it.Lease.Token, lock: it.Lease.Kind == leaseKindLock}, nil
}

// adoptID is the Client's id, or one no lease carries before its first.
func (c *Client) adoptID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.id == "" {
		return "\x00none"
	}
	return c.id
}

// Suspend gives the lease up after a teardown stopped partway: the item goes
// IDLE (keeping its idleSince), marked incomplete, with no lease, so no
// creator waits for it, and the next backstop resumes the teardown (it reads
// Item.Incomplete). Any activity (MarkActive, Mark, AddHold, an active
// Observe), a Claim and a Release clear the mark.
func (l *Lease) Suspend(ctx context.Context) error {
	if l.lock {
		return fmt.Errorf("lifecycle: suspend %s: %w", l.key, ErrNotALock)
	}
	now := l.c.now()
	return l.write(ctx, "suspend", func(cond string, nm map[string]string, vals map[string]types.AttributeValue) error {
		nm["#state"], nm["#idleSince"], nm["#incomplete"] = attrState, attrIdleSince, attrIncomplete
		vals[":idle"], vals[":now"], vals[":true"] = s(string(StateIdle)), s(formatTime(now)), &types.AttributeValueMemberBOOL{Value: true}
		_, err := l.a.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                 aws.String(TableName),
			Key:                       keyOf(l.key),
			UpdateExpression:          aws.String("SET #state = :idle, #idleSince = if_not_exists(#idleSince, :now), #incomplete = :true" + stamp(StateIdle, false, nm, vals) + " REMOVE #lease"),
			ConditionExpression:       aws.String(cond),
			ExpressionAttributeNames:  nm,
			ExpressionAttributeValues: vals,
		})
		return err
	}, true)
}
