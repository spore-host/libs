package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/spore-host/libs/tags"
)

// Attribute names. Every expression refers to them through #placeholders,
// since several (state, step, owner) are DynamoDB reserved words.
const (
	attrPK        = "pk"
	attrState     = "state"
	attrEpoch     = "epoch"
	attrIdleSince = "idleSince"
	attrLease     = "lease"
	attrHolds     = "holds"
	attrTool      = "tool"
	attrScope     = "scope"
	attrToken     = "token"
	attrOwner     = "owner"
	attrHeartbeat = "heartbeat"
	attrStep      = "step"
	attrSince     = "since"
)

// Item is a tool item as read from the table.
type Item struct {
	Tool  tags.Tool
	Scope string
	State State
	// Epoch counts MarkActive and AddHold calls since the item was created.
	// It is 0 for an item that has only ever been observed.
	Epoch int64
	// IdleSince is when the tool was first observed idle, or zero.
	IdleSince time.Time
	// Lease is the teardown claim, or nil.
	Lease *LeaseInfo
	// Holds are the tools that depend on this one in this scope, sorted.
	Holds []tags.Tool
}

// LeaseInfo is a teardown claim as stored in a tool item.
type LeaseInfo struct {
	Token     string
	Owner     string
	Heartbeat time.Time
	Step      string
}

// IdleFor is how long the item has been idle as of now: zero unless it is
// IDLE or TEARDOWN with an idleSince.
func (it Item) IdleFor(now time.Time) time.Duration {
	if (it.State != StateIdle && it.State != StateTeardown) || it.IdleSince.IsZero() {
		return 0
	}
	if d := now.Sub(it.IdleSince); d > 0 {
		return d
	}
	return 0
}

func str(m map[string]types.AttributeValue, k string) string {
	if v, ok := m[k].(*types.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

func decodeItem(m map[string]types.AttributeValue) Item {
	it := Item{
		Tool:      tags.Tool(str(m, attrTool)),
		Scope:     str(m, attrScope),
		State:     State(str(m, attrState)),
		IdleSince: parseTime(str(m, attrIdleSince)),
	}
	if n, ok := m[attrEpoch].(*types.AttributeValueMemberN); ok {
		it.Epoch, _ = strconv.ParseInt(n.Value, 10, 64)
	}
	if l, ok := m[attrLease].(*types.AttributeValueMemberM); ok {
		it.Lease = &LeaseInfo{
			Token:     str(l.Value, attrToken),
			Owner:     str(l.Value, attrOwner),
			Heartbeat: parseTime(str(l.Value, attrHeartbeat)),
			Step:      str(l.Value, attrStep),
		}
	}
	if h, ok := m[attrHolds].(*types.AttributeValueMemberSS); ok {
		for _, v := range h.Value {
			it.Holds = append(it.Holds, tags.Tool(v))
		}
		sort.Slice(it.Holds, func(i, j int) bool { return it.Holds[i] < it.Holds[j] })
	}
	return it
}

func s(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }
func n(v int64) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(v, 10)}
}
func ss(v string) types.AttributeValue { return &types.AttributeValueMemberSS{Value: []string{v}} }

func keyOf(pk string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{attrPK: s(pk)}
}

// names builds an ExpressionAttributeNames map from attribute names, keyed
// "#"+name. Only the names an expression uses may be passed: DynamoDB rejects
// unused ones.
func names(attrs ...string) map[string]string {
	m := make(map[string]string, len(attrs))
	for _, a := range attrs {
		m["#"+a] = a
	}
	return m
}

// epochCond is the condition that an item's epoch is still e. An item that
// has never been activated has no epoch attribute.
func epochCond(e int64, vals map[string]types.AttributeValue) string {
	if e == 0 {
		return "attribute_not_exists(#epoch)"
	}
	vals[":epoch"] = n(e)
	return "#epoch = :epoch"
}

const noLiveLease = "(attribute_not_exists(#lease) OR #lease.#heartbeat < :stale)"

func (c *Client) get(ctx context.Context, a API, key string) (Item, bool, error) {
	out, err := a.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(TableName),
		Key:            keyOf(key),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return Item{}, false, err
	}
	if out == nil || out.Item == nil {
		return Item{}, false, nil
	}
	return decodeItem(out.Item), true, nil
}

// Get reads a tool item. It reports false when the item, or the table, does
// not exist.
func (c *Client) Get(ctx context.Context, tool tags.Tool, scope string) (Item, bool, error) {
	a, _, key, err := c.target(tool, scope)
	if err != nil {
		return Item{}, false, err
	}
	it, ok, err := c.get(ctx, a, key)
	if isNotFound(err) {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("lifecycle: read %s: %w", key, err)
	}
	return it, ok, nil
}

// Errors.
var (
	// ErrWaitTimeout is wrapped by the *WaitError MarkActive and AddHold return
	// when a live teardown lease, the table's closing sentinel, or a table
	// transition did not clear within Options.WaitTimeout.
	ErrWaitTimeout = errors.New("lifecycle: timed out waiting for the lifecycle table")

	// ErrLeaseLost is returned by Lease methods once the lease is no longer
	// this caller's: a creator or another backstop took it over, or the item
	// is gone. The caller must stop deleting at once.
	ErrLeaseLost = errors.New("lifecycle: teardown lease lost")

	// ErrNotClaimable is wrapped by the *NotClaimableError Claim returns when
	// the tool is not (or is no longer) eligible for teardown.
	ErrNotClaimable = errors.New("lifecycle: not claimable")

	// ErrTableClosing is returned by Observe when the table is being deleted.
	// Observe again on the next tick.
	ErrTableClosing = errors.New("lifecycle: the lifecycle table is being deleted")
)

// WaitError is returned when MarkActive or AddHold gives up waiting.
type WaitError struct {
	Tool  tags.Tool
	Scope string
	// Waited is how long the caller waited.
	Waited time.Duration
	// Blocker says what was in the way: "lease" (a teardown in progress),
	// "sentinel" (the table being deleted), "conflict" (contending
	// transactions), or "table <STATUS>".
	Blocker string
	// Owner and Heartbeat describe the blocking lease, when Blocker is "lease".
	Owner     string
	Heartbeat time.Time
}

func (e *WaitError) Error() string {
	msg := fmt.Sprintf("lifecycle: %s/%s: gave up after %s waiting on %s", e.Tool, e.Scope, e.Waited.Round(time.Second), e.Blocker)
	if e.Owner != "" {
		msg += fmt.Sprintf(" held by %s (heartbeat %s)", e.Owner, formatTime(e.Heartbeat))
	}
	return msg
}

// Unwrap returns ErrWaitTimeout.
func (e *WaitError) Unwrap() error { return ErrWaitTimeout }

// NotClaimableError says why Claim refused.
type NotClaimableError struct {
	Tool   tags.Tool
	Scope  string
	Reason string
}

func (e *NotClaimableError) Error() string {
	return fmt.Sprintf("lifecycle: %s/%s not claimable: %s", e.Tool, e.Scope, e.Reason)
}

// Unwrap returns ErrNotClaimable.
func (e *NotClaimableError) Unwrap() error { return ErrNotClaimable }

func isNotFound(err error) bool {
	var e *types.ResourceNotFoundException
	return errors.As(err, &e)
}

func isInUse(err error) bool {
	var e *types.ResourceInUseException
	return errors.As(err, &e)
}

func isCCF(err error) bool {
	var e *types.ConditionalCheckFailedException
	return errors.As(err, &e)
}

// cancellation splits a TransactionCanceledException into per-item results.
// ok is false for any other error.
func cancellation(err error) (reasons []types.CancellationReason, ok bool) {
	var e *types.TransactionCanceledException
	if !errors.As(err, &e) {
		return nil, false
	}
	return e.CancellationReasons, true
}

func reasonCCF(reasons []types.CancellationReason, i int) (map[string]types.AttributeValue, bool) {
	if i >= len(reasons) || aws.ToString(reasons[i].Code) != "ConditionalCheckFailed" {
		return nil, false
	}
	return reasons[i].Item, true
}

// LeaseStale reports whether it carries a teardown lease whose heartbeat is
// older than Options.StaleAfter as of the Client's clock: a teardown whose
// holder is gone, which a creator or backstop may take over. An item with no
// lease is not stale. It reads nothing and writes nothing.
func (c *Client) LeaseStale(it Item) bool {
	return it.Lease != nil && c.isStale(it.Lease.Heartbeat, c.now())
}

// ListItems returns every tool item in region's table, of every tool and
// scope, with a strongly consistent scan; the closing sentinel is left out.
// exists is false when the table does not exist or is not accepting reads
// (CREATING or DELETING). It writes nothing, so a caller in report mode can
// see whether the table is empty without CloseTableIfEmpty's sentinel write.
func (c *Client) ListItems(ctx context.Context, region string) (items []Item, exists bool, err error) {
	a, err := c.api(region)
	if err != nil {
		return nil, false, err
	}
	var start map[string]types.AttributeValue
	for {
		out, err := a.Scan(ctx, &dynamodb.ScanInput{
			TableName:         aws.String(TableName),
			ConsistentRead:    aws.Bool(true),
			ExclusiveStartKey: start,
		})
		if isNotFound(err) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("lifecycle: scan %s in %s: %w", TableName, region, err)
		}
		for _, m := range out.Items {
			if str(m, attrPK) != SentinelKey {
				items = append(items, decodeItem(m))
			}
		}
		if len(out.LastEvaluatedKey) == 0 {
			return items, true, nil
		}
		start = out.LastEvaluatedKey
	}
}
