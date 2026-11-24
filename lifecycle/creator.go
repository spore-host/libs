package lifecycle

import (
	"context"
	"fmt"
	"strings"
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
	_, err := c.activate(ctx, tool, scope, "", "")
	return err
}

// AddHold records that holder depends on tool's resources in scope (for
// example calque using a spawn pool). Until RemoveHold, tool is treated as
// active: Observe counts a hold as activity and Claim refuses. AddHold is
// MarkActive plus the hold, and blocks and fails the same way.
func (c *Client) AddHold(ctx context.Context, tool tags.Tool, scope string, holder tags.Tool) error {
	if err := validate(holder, scope); err != nil {
		return err
	}
	_, err := c.activate(ctx, tool, scope, holder, "")
	return err
}

// RemoveHold removes holder's hold on tool in scope. Removing a hold that does
// not exist, or from an item or table that does not exist, succeeds.
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
	now := c.now()
	for attempt := 1; ; attempt++ {
		_, err = a.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                           aws.String(TableName),
			Key:                                 keyOf(key),
			UpdateExpression:                    aws.String("SET #lastActivityAt = :now DELETE #holds :holder"),
			ConditionExpression:                 aws.String("attribute_exists(#pk) AND " + laCond),
			ExpressionAttributeNames:            names(attrHolds, attrPK, attrLastActive),
			ExpressionAttributeValues:           map[string]types.AttributeValue{":holder": ss(string(holder)), ":now": s(formatTime(now))},
			ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
		})
		la, newer := newerStamp(err, 0, now)
		if !isCCF(err) || !newer || attempt == writeAttempts {
			break
		}
		now = la // a later stamp: the change is stamped there
	}
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

// activate runs the activation. mark, when set, is a creator mark's token;
// the element written (token|time, stamped at commit) is returned, the last
// one tried on an error.
func (c *Client) activate(ctx context.Context, tool tags.Tool, scope string, holder tags.Tool, mark string) (string, error) {
	a, region, key, err := c.target(tool, scope)
	if err != nil {
		return "", err
	}
	element := ""
	start := c.now()
	missing, polls := 0, 0
	var floor time.Time // a later lastActivityAt met (laCond)
	for {
		now := laterOf(c.now(), floor)
		if mark != "" {
			element = markElement(mark, now)
		}
		b, err := c.activateOnce(ctx, a, key, tool, scope, holder, element, now)
		switch {
		case err == nil && b == nil:
			return element, nil
		case isNotFound(err):
			// No table (or one mid-transition): make sure it exists, then
			// try again. A table that keeps vanishing is waited for like any
			// other blocker.
			if err := c.ensureTable(ctx, a, region, tool, scope, start); err != nil {
				return element, err
			}
			if missing++; missing < 3 {
				continue
			}
			b = &blocked{what: "table"}
		case err != nil:
			return element, fmt.Errorf("lifecycle: mark %s active: %w", key, err)
		}
		missing = 0
		if b.what == "lease" {
			// A later stamp, not a live lease: try again at that stamp.
			if it := decodeItem(b.item); it.LastActivityAt.After(now) && (it.Lease == nil || c.isStale(it.Lease.Heartbeat, c.now())) {
				floor = it.LastActivityAt
				continue
			}
		}
		now = c.now()
		if b.what == "sentinel" && !b.sentinel.IsZero() && c.isStale(b.sentinel, now) {
			if err := c.clearStaleSentinel(ctx, a, now); err != nil {
				return element, err
			}
			continue
		}
		// Polls are counted as well as timed, so a clock that stalls cannot
		// make the wait endless.
		if waited := max(now.Sub(start), time.Duration(polls)*c.o.PollInterval); waited >= c.o.WaitTimeout {
			we := &WaitError{Tool: tool, Scope: scope, Waited: waited, Blocker: b.what}
			if b.what == "lease" {
				if l := decodeItem(b.item).Lease; l != nil {
					we.Owner, we.Heartbeat = l.Owner, l.Heartbeat
				}
			}
			return element, we
		}
		polls++
		if err := c.o.Sleep(ctx, c.o.PollInterval); err != nil {
			return element, err
		}
	}
}

// activateOnce runs the activation transaction once. It returns a non-nil
// *blocked when the transaction was cancelled by the sentinel, a live lease,
// or a conflicting transaction.
func (c *Client) activateOnce(ctx context.Context, a API, key string, tool tags.Tool, scope string, holder tags.Tool, mark string, now time.Time) (*blocked, error) {
	nm := names(attrState, attrEpoch, attrTool, attrScope, attrIdleSince, attrLease, attrHeartbeat, attrIncomplete, attrActiveAt, attrLastActive)
	vals := map[string]types.AttributeValue{
		":active": s(string(StateActive)),
		":zero":   n(0),
		":one":    n(1),
		":tool":   s(string(tool)),
		":scope":  s(scope),
		":stale":  s(c.staleBefore(now)),
	}
	upd := "SET #state = :active, #epoch = if_not_exists(#epoch, :zero) + :one, #tool = :tool, #scope = :scope, #activeAt = :now, " +
		"#lastActivityAt = :now" + stamp(StateActive, true, nm, vals) + " REMOVE #idleSince, #lease, #incomplete"
	vals[":now"] = s(formatTime(now))
	switch {
	case holder != "":
		// A hold is its own "in use" (RemoveHold ends it).
	case mark == "":
		// MarkActive: activity with no end of its own, in use for the
		// reader's mark bound.
		upd = strings.Replace(upd, " REMOVE ", ", #pulseAt = :now REMOVE ", 1)
		nm["#pulseAt"] = attrPulseAt
	default:
		upd += " ADD #marks :mark"
		nm["#marks"] = attrMarks
		vals[":mark"] = ss(mark)
	}
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
			ConditionExpression:                 aws.String(noLiveLease + " AND " + laCond),
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
