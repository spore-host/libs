package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/spore-host/libs/tags"
)

// Observe records what a backstop saw this tick: active is true when any of
// tool's activity signals in scope is live. Only a component that can see
// every activity signal of the tool may call it.
//
// A hold on the tool counts as activity whatever active says. When the tool is
// active, Observe sets state=ACTIVE and removes idleSince and any lease (so a
// teardown in progress loses its lease and stops at its next Step). When it is
// idle, Observe sets state=IDLE and, if not already set, idleSince=now,
// conditional on the epoch it read: if a creator ran MarkActive in between,
// nothing is written and the fresh item is returned. A teardown holding a live
// lease is left alone, and so is an item with a creator mark younger than
// Options.MarkBound (a creator the backstop cannot see is running): an idle
// Observe then records nothing. Older marks (creators that died) are removed
// along with ACTIVE. An active Observe stamps lastActivityAt every time.
// Observe creates the table and the item if they do not exist.
//
// It returns the item as it now stands and how long the tool has been idle.
// While the table is being deleted (a live closer's sentinel) it returns
// ErrTableClosing at once; observe again on the next tick. A stale sentinel
// (older than Options.StaleAfter, its closer dead or stalled, or one with no
// time: see CloseTableIfEmpty) Observe removes, as MarkActive and
// SetIdleIfUnchanged do, and observes again at once. One that is back is
// removed once more and observed past after one Options.PollInterval; a third
// is ErrTableClosing. So a sentinel adds at most one PollInterval to an
// Observe.
func (c *Client) Observe(ctx context.Context, tool tags.Tool, scope string, active bool) (Item, time.Duration, error) {
	a, region, key, err := c.target(tool, scope)
	if err != nil {
		return Item{}, 0, err
	}
	start := c.now()
	missing, cleared := 0, 0
	for {
		closing, err := c.observeOnce(ctx, a, key, tool, scope, active)
		var it Item
		if err == nil && closing == nil {
			it, _, err = c.get(ctx, a, key)
		}
		if isNotFound(err) && missing < 2 {
			// No table, or it was deleted under us: create it and observe
			// again.
			missing++
			if err := c.ensureTable(ctx, a, region, tool, scope, start); err != nil {
				return Item{}, 0, err
			}
			continue
		}
		if closing != nil && c.sentinelStale(closing.item, c.now()) && cleared < 2 {
			// A closer that died left it (libs#88): remove it and observe
			// again, at once the first time and a poll later the second.
			// Each clear counts toward the bound; one that finds a live
			// closer's sentinel in its place is ErrTableClosing at once.
			cleared++
			clear, err := c.clearStaleSentinel(ctx, a, c.now())
			if err != nil {
				return Item{}, 0, fmt.Errorf("lifecycle: observe %s: %w", key, err)
			}
			if clear {
				if cleared > 1 {
					if err := c.again(ctx); err != nil {
						return Item{}, 0, fmt.Errorf("lifecycle: observe %s: %w", key, err)
					}
				}
				start = c.now() // a table's wait counts from here
				continue
			}
		}
		if closing != nil {
			err = ErrTableClosing
		}
		if err != nil {
			return Item{}, 0, fmt.Errorf("lifecycle: observe %s: %w", key, err)
		}
		return it, it.IdleFor(c.now()), nil
	}
}

// activeStampFresh is how recent an active Observe leaves lastActivityAt
// alone, when only the stamp would change.
const activeStampFresh = time.Minute

// observeOnce runs one observation. It returns a non-nil *blocked (a
// "sentinel") when the closing sentinel cancelled the write: the table is
// closing.
func (c *Client) observeOnce(ctx context.Context, a API, key string, tool tags.Tool, scope string, active bool) (*blocked, error) {
	it, exists, err := c.get(ctx, a, key)
	if err != nil {
		return nil, err
	}
	now := c.now()
	vals := map[string]types.AttributeValue{":tool": s(string(tool)), ":scope": s(scope)}
	var upd, cond string
	var nm map[string]string
	if active || len(it.Holds) > 0 {
		if exists && it.State == StateActive && !untimed(it) && it.Lease == nil && it.IdleSince.IsZero() && !it.Incomplete &&
			!it.LastActivityAt.IsZero() && now.Sub(it.LastActivityAt) < activeStampFresh {
			// Only the stamp would change, and it is fresh: no write (a
			// reader's IdleSince is off by under activeStampFresh).
			return nil, nil
		}
		// Recorded otherwise, so a reader's IdleSince counts from the last
		// activity seen.
		nm = names(attrState, attrTool, attrScope, attrIdleSince, attrLease, attrIncomplete, attrLastActive)
		vals[":active"] = s(string(StateActive))
		vals[":now"] = s(formatTime(now))
		upd = "SET #state = :active, #tool = :tool, #scope = :scope, #lastActivityAt = :now" + stamp(StateActive, false, nm, vals) +
			" REMOVE #idleSince, #lease, #incomplete"
	} else {
		if exists && it.Lease != nil && !c.leaseStale(it.Lease, now) {
			return nil, nil // a teardown is in progress
		}
		if exists && it.State == StateIdle && !it.IdleSince.IsZero() && it.Lease == nil {
			return nil, nil // already idle; keep the first idleSince
		}
		if exists && liveMark(it, now, c.o.MarkBound) {
			// A creator is running (one the backstop cannot see): record
			// nothing. A mark older than MarkBound is a creator that died,
			// and is removed with the rest.
			return nil, nil
		}
		nm = names(attrState, attrIdleSince, attrTool, attrScope, attrLease, attrEpoch, attrHolds, attrHeartbeat, attrMarks)
		vals[":idle"] = s(string(StateIdle))
		vals[":now"] = s(formatTime(now))
		vals[":stale"] = s(c.staleBefore(now))
		upd = "SET #state = :idle, #idleSince = if_not_exists(#idleSince, :now), #tool = :tool, #scope = :scope" +
			stamp(StateIdle, false, nm, vals) + " REMOVE #lease, #marks"
		// As read: the epoch (an activation), and lastActivityAt (any other
		// change: a mark refreshed or ended, a hold removed, a release).
		cond = epochCond(it.Epoch, vals) + " AND " + laEqual(it.LastActivityAt, nm, vals) + " AND attribute_not_exists(#holds) AND " + noLiveLease
	}
	u := &types.Update{
		TableName:                 aws.String(TableName),
		Key:                       keyOf(key),
		UpdateExpression:          aws.String(upd),
		ExpressionAttributeNames:  nm,
		ExpressionAttributeValues: vals,
	}
	if cond != "" {
		u.ConditionExpression = aws.String(cond)
	}
	_, err = a.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{sentinelAbsent(), {Update: u}}})
	if err == nil {
		return nil, nil
	}
	b, err := classifyCancel(err)
	switch {
	case err != nil:
		return nil, err
	case b.what == "sentinel":
		return b, nil
	case b.what == "lease":
		// The item changed since it was read: a creator bumped the epoch,
		// a hold appeared, or a teardown claimed it. Whatever happened is
		// newer than this observation, so record nothing.
		return nil, nil
	default:
		return nil, errors.New("transaction conflict")
	}
}

// Claim takes the teardown lease on tool in scope, once the tool has been
// idle for at least grace. It sets state=TEARDOWN and a fresh lease, on
// condition that the item is still IDLE at the epoch it read, idleSince is
// at least grace old, no hold exists, and no live lease exists.
//
// After a successful Claim the caller must re-read its activity signals; if
// any is live it calls Lease.Release. Otherwise it deletes, calling
// Lease.Step before every delete, and Lease.Finish at the end.
//
// A refusal is a *NotClaimableError (errors.Is ErrNotClaimable).
func (c *Client) Claim(ctx context.Context, tool tags.Tool, scope string, grace time.Duration) (*Lease, error) {
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
	}
	now := c.now()
	switch {
	case len(it.Holds) > 0:
		return nil, refuse(fmt.Sprintf("held by %v", it.Holds))
	case it.Lease != nil && !c.leaseStale(it.Lease, now):
		return nil, refuse("lease held by " + it.Lease.Owner)
	case liveMark(it, now, c.o.MarkBound):
		// A creator still running (a mark an older backstop's idle Observe
		// left): the condition pins lastActivityAt, so this read decides.
		return nil, refuse("a creator's mark is live")
	case it.State != StateIdle || it.IdleSince.IsZero():
		return nil, refuse("state is " + string(it.State))
	case it.IdleFor(now) < grace:
		return nil, refuse(fmt.Sprintf("idle for %s of %s", it.IdleFor(now).Round(time.Second), grace))
	}
	token, err := c.newToken()
	if err != nil {
		return nil, err
	}
	id, err := c.clientID()
	if err != nil {
		return nil, err
	}
	vals := map[string]types.AttributeValue{
		":teardown": s(string(StateTeardown)),
		":idle":     s(string(StateIdle)),
		":cutoff":   s(formatTime(now.Add(-grace))),
		":stale":    s(c.staleBefore(now)),
		":lease": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
			attrToken:     s(token),
			attrOwner:     s(c.o.Owner),
			attrClient:    s(id),
			attrHeartbeat: s(formatTime(now)),
			attrStep:      s(""),
		}},
	}
	nm := names(attrState, attrLease, attrEpoch, attrIdleSince, attrHolds, attrHeartbeat, attrIncomplete)
	cond := "#state = :idle AND " + epochCond(it.Epoch, vals) + " AND " + laEqual(it.LastActivityAt, nm, vals) +
		" AND #idleSince <= :cutoff AND attribute_not_exists(#holds) AND " + noLiveLease
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

// Lease is a teardown claim on one tool and scope. Its token fences every
// write: once anyone else has changed the lease, every method returns an
// error wrapping ErrLeaseLost. It is safe for concurrent use.
type Lease struct {
	c     *Client
	a     API
	key   string
	tool  tags.Tool
	scope string
	token string
	// lock: taken by LockIfUnchanged (Unlock gives it back); otherwise a
	// teardown claim.
	lock bool

	mu   sync.Mutex
	done bool
}

// Token is the lease's fencing token.
func (l *Lease) Token() string { return l.token }

// Tool is the tool the lease is on.
func (l *Lease) Tool() tags.Tool { return l.tool }

// Scope is the scope the lease is on.
func (l *Lease) Scope() string { return l.scope }

// Step records that the holder is about to perform the delete step name, and
// refreshes the heartbeat. Call it before every delete; on error, delete
// nothing more.
func (l *Lease) Step(ctx context.Context, name string) error {
	now := l.c.now()
	return l.write(ctx, "step "+name, func(cond string, nm map[string]string, vals map[string]types.AttributeValue) error {
		nm["#heartbeat"], nm["#step"] = attrHeartbeat, attrStep
		vals[":now"], vals[":step"] = s(formatTime(now)), s(name)
		_, err := l.a.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                 aws.String(TableName),
			Key:                       keyOf(l.key),
			UpdateExpression:          aws.String("SET #lease.#heartbeat = :now, #lease.#step = :step"),
			ConditionExpression:       aws.String(cond),
			ExpressionAttributeNames:  nm,
			ExpressionAttributeValues: vals,
		})
		return err
	}, false)
}

// Release gives the lease up because activity was found: the item goes back
// to ACTIVE and idleSince is removed, so the grace period starts over.
//
// A lock's lease (LockIfUnchanged) it refuses with ErrNotALock: give a lock
// back with Unlock.
func (l *Lease) Release(ctx context.Context) error {
	if l.lock {
		return fmt.Errorf("lifecycle: release %s: %w", l.key, ErrNotALock)
	}
	return l.write(ctx, "release", func(cond string, nm map[string]string, vals map[string]types.AttributeValue) error {
		nm["#state"], nm["#idleSince"], nm["#incomplete"], nm["#lastActivityAt"] = attrState, attrIdleSince, attrIncomplete, attrLastActive
		vals[":active"], vals[":now"] = s(string(StateActive)), s(formatTime(l.c.now()))
		_, err := l.a.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName: aws.String(TableName),
			Key:       keyOf(l.key),
			UpdateExpression: aws.String("SET #state = :active, #lastActivityAt = :now" + stamp(StateActive, false, nm, vals) +
				" REMOVE #lease, #idleSince, #incomplete"),
			ConditionExpression:       aws.String(cond),
			ExpressionAttributeNames:  nm,
			ExpressionAttributeValues: vals,
		})
		return err
	}, true)
}

// Finish deletes the tool item once teardown is complete. Call
// CloseTableIfEmpty after it.
//
// A lock's lease (LockIfUnchanged) it refuses with ErrNotALock: a lock never
// deletes the item; give it back with Unlock.
func (l *Lease) Finish(ctx context.Context) error {
	if l.lock {
		return fmt.Errorf("lifecycle: finish %s: %w", l.key, ErrNotALock)
	}
	return l.write(ctx, "finish", func(cond string, nm map[string]string, vals map[string]types.AttributeValue) error {
		_, err := l.a.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName:                 aws.String(TableName),
			Key:                       keyOf(l.key),
			ConditionExpression:       aws.String(cond),
			ExpressionAttributeNames:  nm,
			ExpressionAttributeValues: vals,
		})
		return err
	}, true)
}

// write runs one token-fenced write. final marks the lease done on success.
func (l *Lease) write(ctx context.Context, what string, do func(cond string, nm map[string]string, vals map[string]types.AttributeValue) error, final bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done {
		return fmt.Errorf("lifecycle: %s %s: lease already released or finished: %w", what, l.key, ErrLeaseLost)
	}
	err := do("#lease.#token = :token", names(attrLease, attrToken), map[string]types.AttributeValue{":token": s(l.token)})
	switch {
	case err == nil:
		if final {
			l.done = true
		}
		return nil
	case isCCF(err), isNotFound(err):
		l.done = true
		return fmt.Errorf("lifecycle: %s %s: %w", what, l.key, ErrLeaseLost)
	default:
		return fmt.Errorf("lifecycle: %s %s: %w", what, l.key, err)
	}
}
