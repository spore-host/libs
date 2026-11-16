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
// lease is left alone. Observe creates the table and the item if they do not
// exist.
//
// It returns the item as it now stands and how long the tool has been idle.
// While the table is being deleted it returns ErrTableClosing; observe again
// on the next tick.
func (c *Client) Observe(ctx context.Context, tool tags.Tool, scope string, active bool) (Item, time.Duration, error) {
	a, region, key, err := c.target(tool, scope)
	if err != nil {
		return Item{}, 0, err
	}
	start := c.now()
	for attempt := 1; ; attempt++ {
		err := c.observeOnce(ctx, a, key, tool, scope, active)
		var it Item
		if err == nil {
			it, _, err = c.get(ctx, a, key)
		}
		if isNotFound(err) && attempt < 3 {
			// No table, or it was deleted under us: create it and observe
			// again.
			if err := c.ensureTable(ctx, a, region, tool, scope, start); err != nil {
				return Item{}, 0, err
			}
			continue
		}
		if err != nil {
			return Item{}, 0, fmt.Errorf("lifecycle: observe %s: %w", key, err)
		}
		return it, it.IdleFor(c.now()), nil
	}
}

func (c *Client) observeOnce(ctx context.Context, a API, key string, tool tags.Tool, scope string, active bool) error {
	it, exists, err := c.get(ctx, a, key)
	if err != nil {
		return err
	}
	now := c.now()
	vals := map[string]types.AttributeValue{":tool": s(string(tool)), ":scope": s(scope)}
	var upd, cond string
	var nm map[string]string
	if active || len(it.Holds) > 0 {
		if exists && it.State == StateActive && it.IdleSince.IsZero() && it.Lease == nil {
			return nil // nothing to record
		}
		upd = "SET #state = :active, #tool = :tool, #scope = :scope REMOVE #idleSince, #lease"
		nm = names(attrState, attrTool, attrScope, attrIdleSince, attrLease)
		vals[":active"] = s(string(StateActive))
	} else {
		if exists && it.Lease != nil && !c.isStale(it.Lease.Heartbeat, now) {
			return nil // a teardown is in progress
		}
		if exists && it.State == StateIdle && !it.IdleSince.IsZero() && it.Lease == nil {
			return nil // already idle; keep the first idleSince
		}
		upd = "SET #state = :idle, #idleSince = if_not_exists(#idleSince, :now), #tool = :tool, #scope = :scope REMOVE #lease"
		cond = epochCond(it.Epoch, vals) + " AND attribute_not_exists(#holds) AND " + noLiveLease
		nm = names(attrState, attrIdleSince, attrTool, attrScope, attrLease, attrEpoch, attrHolds, attrHeartbeat)
		vals[":idle"] = s(string(StateIdle))
		vals[":now"] = s(formatTime(now))
		vals[":stale"] = s(c.staleBefore(now))
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
		return nil
	}
	b, err := classifyCancel(err)
	switch {
	case err != nil:
		return err
	case b.what == "sentinel":
		return ErrTableClosing
	case b.what == "lease":
		// The item changed since it was read: a creator bumped the epoch,
		// a hold appeared, or a teardown claimed it. Whatever happened is
		// newer than this observation, so record nothing.
		return nil
	default:
		return errors.New("transaction conflict")
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
	case it.Lease != nil && !c.isStale(it.Lease.Heartbeat, now):
		return nil, refuse("lease held by " + it.Lease.Owner)
	case it.State != StateIdle || it.IdleSince.IsZero():
		return nil, refuse("state is " + string(it.State))
	case it.IdleFor(now) < grace:
		return nil, refuse(fmt.Sprintf("idle for %s of %s", it.IdleFor(now).Round(time.Second), grace))
	}
	token, err := c.newToken()
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
			attrHeartbeat: s(formatTime(now)),
			attrStep:      s(""),
		}},
	}
	cond := "#state = :idle AND " + epochCond(it.Epoch, vals) +
		" AND #idleSince <= :cutoff AND attribute_not_exists(#holds) AND " + noLiveLease
	_, err = a.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(TableName),
		Key:                       keyOf(key),
		UpdateExpression:          aws.String("SET #state = :teardown, #lease = :lease"),
		ConditionExpression:       aws.String(cond),
		ExpressionAttributeNames:  names(attrState, attrLease, attrEpoch, attrIdleSince, attrHolds, attrHeartbeat),
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
func (l *Lease) Release(ctx context.Context) error {
	return l.write(ctx, "release", func(cond string, nm map[string]string, vals map[string]types.AttributeValue) error {
		nm["#state"], nm["#idleSince"] = attrState, attrIdleSince
		vals[":active"] = s(string(StateActive))
		_, err := l.a.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                 aws.String(TableName),
			Key:                       keyOf(l.key),
			UpdateExpression:          aws.String("SET #state = :active REMOVE #lease, #idleSince"),
			ConditionExpression:       aws.String(cond),
			ExpressionAttributeNames:  nm,
			ExpressionAttributeValues: vals,
		})
		return err
	}, true)
}

// Finish deletes the tool item once teardown is complete. Call
// CloseTableIfEmpty after it.
func (l *Lease) Finish(ctx context.Context) error {
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
