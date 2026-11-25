package lifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/spore-host/libs/tags"
)

// MarkActive records that tool is about to create resources in scope. Call it
// before every operation that creates something (setup, deploy, watch,
// launch, pool create, egress create), then create, idempotently.
//
// It makes sure the table exists (waiting out a DELETING table, then creating
// it), then in one transaction checks that the closing sentinel is absent and
// sets state=ACTIVE and epoch+1, removing idleSince and any stale lease, on
// condition that no live lease exists. While a live teardown lease or the
// sentinel is in the way it retries every Options.PollInterval, and after
// Options.WaitTimeout returns a *WaitError (errors.Is ErrWaitTimeout). A
// sentinel older than Options.StaleAfter was abandoned by a crashed closer;
// MarkActive removes it and carries on.
//
// A nil error means no teardown of this tool and scope can be deleting
// resources: one that claims later must first see the tool idle for its
// grace period, and one that held a lease before has lost it.
func (c *Client) MarkActive(ctx context.Context, tool tags.Tool, scope string) error {
	return c.activate(ctx, tool, scope, "", "")
}

// AddHold records that holder depends on tool's resources in scope (for
// example calque using a spawn pool). Until RemoveHold, tool is treated as
// active: Observe counts a hold as activity and Claim refuses. AddHold is
// MarkActive plus the hold, and blocks and fails the same way.
func (c *Client) AddHold(ctx context.Context, tool tags.Tool, scope string, holder tags.Tool) error {
	if err := validate(holder, scope); err != nil {
		return err
	}
	return c.activate(ctx, tool, scope, holder, "")
}

// RemoveHold removes holder's hold on tool in scope, and stamps
// lastActivityAt. Removing a hold that does not exist, or from an item or
// table that does not exist, succeeds and writes nothing.
func (c *Client) RemoveHold(ctx context.Context, tool tags.Tool, scope string, holder tags.Tool) error {
	if err := validate(holder, scope); err != nil {
		return err
	}
	a, _, key, err := c.target(tool, scope)
	if err != nil {
		return err
	}
	// Only the hold goes, and the time of the change: whether the item is
	// still in use is the reader's to judge (InUse), so nothing is left
	// stuck ACTIVE by a second write that did not happen.
	_, err = a.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(TableName),
		Key:                       keyOf(key),
		UpdateExpression:          aws.String("SET #lastActivityAt = :now DELETE #holds :holder"),
		ConditionExpression:       aws.String("contains(#holds, :one)"),
		ExpressionAttributeNames:  names(attrHolds, attrLastActive),
		ExpressionAttributeValues: map[string]types.AttributeValue{":holder": ss(string(holder)), ":one": s(string(holder)), ":now": s(formatTime(c.now()))},
	})
	if err == nil || isCCF(err) || isNotFound(err) {
		return nil
	}
	return fmt.Errorf("lifecycle: remove hold %s on %s: %w", holder, key, err)
}

// blocked is why one activation attempt could not commit.
type blocked struct {
	what     string // "lease", "sentinel" or "conflict"
	item     map[string]types.AttributeValue
	sentinel time.Time // the sentinel's since, when what == "sentinel"
}

// activate runs the activation. mark, when set, is a creator mark's token,
// written with the time of the attempt that commits.
func (c *Client) activate(ctx context.Context, tool tags.Tool, scope string, holder tags.Tool, mark string) error {
	a, region, key, err := c.target(tool, scope)
	if err != nil {
		return err
	}
	start := c.now()
	missing, polls := 0, 0
	newMap := false // the item has no marks map yet: write the whole map
	quick := false  // the last retry went at once
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := c.now()
		b, err := c.activateOnce(ctx, a, key, tool, scope, holder, mark, newMap, now)
		switch {
		case err == nil && b == nil:
			return nil
		case isNotFound(err):
			// No table (or one mid-transition): make sure it exists, then
			// try again. A table that keeps vanishing is waited for like any
			// other blocker.
			if err := c.ensureTable(ctx, a, region, tool, scope, start); err != nil {
				return err
			}
			if missing++; missing < 3 {
				continue
			}
			b = &blocked{what: "table"}
		case err != nil:
			return fmt.Errorf("lifecycle: mark %s active: %w", key, err)
		}
		missing = 0
		// Every retry below counts toward the wait. One that has a cause
		// it can remove (a marks map not as the write assumed, a dead
		// closer's sentinel) goes at once, but never twice in a row.
		retryNow := false
		switch {
		case b.what == "lease" && mark != "":
			if old := decodeItem(b.item); old.Lease == nil || c.leaseStale(old.Lease, now) {
				v, present := b.item[attrMarks]
				if _, isMap := v.(*types.AttributeValueMemberM); present && !isMap {
					return fmt.Errorf("lifecycle: mark %s active: its marks attribute is not a map", key)
				}
				if newMap == present {
					// The map's presence contradicts the write: write it
					// as the item has it.
					newMap, retryNow = !present, true
				}
			}
		case b.what == "sentinel" && !b.sentinel.IsZero() && c.isStale(b.sentinel, now):
			if err := c.clearStaleSentinel(ctx, a, now); err != nil {
				return err
			}
			retryNow = true
		}
		now = c.now()
		polls++
		if retryNow && !quick {
			// Its own cause removed: at once, once, even at the end of the
			// wait (it is not waiting on anything).
			quick = true
			continue
		}
		quick = false
		// Polls are counted as well as timed, so a clock that stalls cannot
		// make the wait endless.
		if waited := max(now.Sub(start), time.Duration(polls-1)*c.o.PollInterval); waited >= c.o.WaitTimeout {
			we := &WaitError{Tool: tool, Scope: scope, Waited: waited, Blocker: b.what}
			if b.what == "lease" {
				if l := decodeItem(b.item).Lease; l != nil {
					we.Owner, we.Heartbeat = l.Owner, l.Heartbeat
				}
			}
			return we
		}
		if err := c.o.Sleep(ctx, c.o.PollInterval); err != nil {
			return err
		}
	}
}

// activateOnce runs the activation transaction once. It returns a non-nil
// *blocked when the transaction was cancelled by the sentinel, a live lease
// (or, for a mark, a marks map not as newMap assumed), or a conflicting
// transaction.
func (c *Client) activateOnce(ctx context.Context, a API, key string, tool tags.Tool, scope string, holder tags.Tool, mark string, newMap bool, now time.Time) (*blocked, error) {
	nm := names(attrState, attrEpoch, attrTool, attrScope, attrIdleSince, attrLease, attrHeartbeat, attrIncomplete, attrActiveAt, attrLastActive)
	vals := map[string]types.AttributeValue{
		":active": s(string(StateActive)),
		":zero":   n(0),
		":one":    n(1),
		":tool":   s(string(tool)),
		":scope":  s(scope),
		":stale":  s(c.staleBefore(now)),
		":now":    s(formatTime(now)),
	}
	set := "SET #state = :active, #epoch = if_not_exists(#epoch, :zero) + :one, #tool = :tool, #scope = :scope, #activeAt = :now, " +
		"#lastActivityAt = :now" + stamp(StateActive, true, nm, vals)
	cond := noLiveLease
	switch {
	case holder != "":
		// A hold is its own "in use" (RemoveHold ends it).
	case mark == "":
		// MarkActive: activity with no end of its own, in use for the
		// reader's mark bound.
		set += ", #pulseAt = :now"
		nm["#pulseAt"] = attrPulseAt
	case newMap:
		nm["#marks"] = attrMarks
		vals[":marks"] = &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{mark: s(formatTime(now))}}
		set += ", #marks = :marks"
		cond += " AND attribute_not_exists(#marks)"
	default:
		nm["#marks"], nm["#tok"] = attrMarks, mark
		set += ", #marks.#tok = :now"
		cond += " AND attribute_exists(#marks)"
	}
	upd := set + " REMOVE #idleSince, #lease, #incomplete"
	if holder != "" {
		upd += " ADD #holds :holder"
		nm["#holds"] = attrHolds
		vals[":holder"] = ss(string(holder))
	}
	_, err := a.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		sentinelAbsent(),
		{Update: &types.Update{
			TableName:                           aws.String(TableName),
			Key:                                 keyOf(key),
			UpdateExpression:                    aws.String(upd),
			ConditionExpression:                 aws.String(cond),
			ExpressionAttributeNames:            nm,
			ExpressionAttributeValues:           vals,
			ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
		}},
	}})
	if err == nil {
		return nil, nil
	}
	return classifyCancel(err)
}

// sentinelAbsent is the transaction item that fails if the table is closing.
func sentinelAbsent() types.TransactWriteItem {
	return types.TransactWriteItem{ConditionCheck: &types.ConditionCheck{
		TableName:                           aws.String(TableName),
		Key:                                 keyOf(SentinelKey),
		ConditionExpression:                 aws.String("attribute_not_exists(#pk)"),
		ExpressionAttributeNames:            names(attrPK),
		ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
	}}
}

// classifyCancel turns the cancellation of a [sentinelAbsent, update]
// transaction into a *blocked. Any other error is returned as is.
func classifyCancel(err error) (*blocked, error) {
	reasons, ok := cancellation(err)
	if !ok {
		return nil, err
	}
	if item, ok := reasonCCF(reasons, 0); ok {
		return &blocked{what: "sentinel", item: item, sentinel: parseTime(str(item, attrSince))}, nil
	}
	if item, ok := reasonCCF(reasons, 1); ok {
		return &blocked{what: "lease", item: item}, nil
	}
	// TransactionConflict, ThrottlingError and the like: retry after a poll.
	return &blocked{what: "conflict"}, nil
}

// clearStaleSentinel removes a sentinel whose writer has been gone for longer
// than Options.StaleAfter. The condition on its age means a fresh sentinel,
// written since by a live closer, is left alone.
func (c *Client) clearStaleSentinel(ctx context.Context, a API, now time.Time) error {
	_, err := a.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName:                 aws.String(TableName),
		Key:                       keyOf(SentinelKey),
		ConditionExpression:       aws.String("#since < :stale"),
		ExpressionAttributeNames:  names(attrSince),
		ExpressionAttributeValues: map[string]types.AttributeValue{":stale": s(c.staleBefore(now))},
	})
	if err == nil || isCCF(err) || isNotFound(err) {
		return nil
	}
	return fmt.Errorf("lifecycle: remove stale closing sentinel: %w", err)
}
