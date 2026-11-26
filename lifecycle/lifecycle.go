// Package lifecycle is the shared-account lease that lets spore.host tools tear
// down what they created without racing a user who is setting up at the same
// moment.
//
// It is part of leave-no-trace (spore-host/spore-host#752): every tool removes
// what it created once nothing it manages has been active for a grace period
// (24 h). In a shared account one user's teardown must never delete what
// another user's setup just created. This package arbitrates that with one
// DynamoDB table per region, spore-host-lifecycle, plus the same table in a
// control region for global scope.
//
// # Items
//
// The tool item, pk "tool#<tool>#<scope>" (scope is a region or "global"):
//
//	state      ACTIVE | IDLE | TEARDOWN
//	epoch      number, +1 on every MarkActive and AddHold
//	idleSince  RFC 3339; set by the first idle Observe, removed by any activity
//	lease      {token, owner, client, heartbeat, step}: the teardown claim
//	holds      string set: tools that depend on this one in this scope
//	marks      map token -> RFC 3339: one per running creator (Mark, refreshed by RefreshMark, until MarkDone)
//	activeAt   RFC 3339; the last activation
//	pulseAt    RFC 3339; the last MarkActive
//	lastActivityAt  RFC 3339; every write that changes what keeps the item in use stamps it (the change detector)
//	stampState, stampEpoch  the state and epoch this package's last state write left
//	incomplete bool: a teardown suspended partway (Lease.Suspend)
//	tool, scope  the item's own tool and scope, for readability
//
// Writers only add and remove what keeps an item in use, and stamp when;
// none sets an item IDLE as a side effect. Readers judge with InUse and
// IdleSince, which for them are authoritative over the state field.
//
// The sentinel, pk "table#closing", exists only while CloseTableIfEmpty is
// deleting the table.
//
// # Creators
//
// Every operation that creates resources (setup, deploy, watch, launch, pool
// create, egress create) calls MarkActive first and creates its resources,
// idempotently, after it returns. MarkActive, in one transaction, checks that
// the sentinel is absent and sets state=ACTIVE, epoch+1, removes idleSince and
// any stale lease, on condition that no live lease exists. If a live lease or
// the sentinel is in the way it polls (every 10 s, for up to 10 min) and
// returns an error wrapping ErrWaitTimeout if the way never clears.
//
// # The backstop
//
// Only a component that sees every activity signal of a tool may declare it
// idle. It calls Observe every tick; once the tool has been idle for the
// grace it calls Claim, re-reads its activity signals, and either calls
// Lease.Release (activity appeared) or deletes the footprint, calling
// Lease.Step before each delete and Lease.Finish at the end, then
// CloseTableIfEmpty. A lease whose heartbeat is 20 min old is stale: any
// creator or backstop may take it over, and its holder's next Step fails with
// ErrLeaseLost.
//
// # What it guarantees, and what it assumes
//
//   - No MarkActive succeeds while a live lease exists, so no creator proceeds
//     while a teardown that holds its lease is deleting.
//   - A creator that succeeds after a Claim (only possible once the lease is
//     stale, released or gone) makes the claimant's next Step or Finish fail
//     with ErrLeaseLost: MarkActive removes the lease, so the fencing token no
//     longer matches.
//   - A Claim succeeds only when idleSince is at least grace old, and every
//     MarkActive removes idleSince, so a Claim is always at least grace after
//     the last MarkActive.
//   - The table is never deleted while a tool item exists: every write that can
//     create an item checks the sentinel in the same transaction, and
//     CloseTableIfEmpty writes the sentinel before its consistent scan.
//
// It assumes that a creator creates its resources within the grace period of
// its MarkActive (call MarkActive again before any later create), that a
// teardown's time from a Step to the delete that follows it is well under
// 20 min, and that the participants' clocks agree to within minutes: leases,
// grace and staleness compare RFC 3339 timestamps written by different hosts.
// DynamoDB has no conditional DeleteTable, so CloseTableIfEmpty's window from
// its scan to its DeleteTable is protected by the sentinel, which any writer
// treats as abandoned once it is 20 min old: the closer reads its sentinel
// again just before DeleteTable, and deletes only while it is still its own
// and under StaleAfter/2 old (10 min by default), leaving one round trip
// unprotected.
package lifecycle

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/spore-host/libs/tags"
)

// TableName is the lifecycle table's name in every region.
const TableName = "spore-host-lifecycle"

// ScopeGlobal is the scope of a tool's account-wide resources (IAM roles, the
// control-region machinery). Its item lives in the control region's table.
const ScopeGlobal = "global"

// SentinelKey is the pk of the item that exists only while the table is being
// deleted.
const SentinelKey = "table#closing"

// Defaults for Options.
const (
	DefaultPollInterval      = 10 * time.Second
	DefaultWaitTimeout       = 10 * time.Minute
	DefaultStaleAfter        = 20 * time.Minute
	DefaultTablePollInterval = 2 * time.Second
	DefaultMarkBound         = time.Hour
)

// State is a tool item's state.
type State string

// The states of a tool item.
const (
	StateActive   State = "ACTIVE"
	StateIdle     State = "IDLE"
	StateTeardown State = "TEARDOWN"
)

// API is the subset of the DynamoDB client this package uses. A
// *dynamodb.Client satisfies it; so does lifecycletest.Fake.
type API interface {
	DescribeTable(context.Context, *dynamodb.DescribeTableInput, ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
	CreateTable(context.Context, *dynamodb.CreateTableInput, ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error)
	DeleteTable(context.Context, *dynamodb.DeleteTableInput, ...func(*dynamodb.Options)) (*dynamodb.DeleteTableOutput, error)
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	UpdateItem(context.Context, *dynamodb.UpdateItemInput, ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	DeleteItem(context.Context, *dynamodb.DeleteItemInput, ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
	TransactWriteItems(context.Context, *dynamodb.TransactWriteItemsInput, ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
	Scan(context.Context, *dynamodb.ScanInput, ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
}

var _ API = (*dynamodb.Client)(nil)

// Options configures a Client. DynamoDB is required; everything else has a
// default.
type Options struct {
	// DynamoDB returns the client for a region. It is called once per region
	// and the result is cached.
	DynamoDB func(region string) (API, error)

	// ControlRegion holds the items of ScopeGlobal. It is required only to use
	// that scope.
	ControlRegion string

	// Owner identifies this caller in a lease and in the table's tags (for
	// example "lagotto-poller/us-east-1" or a user name). Default "unknown".
	Owner string

	// Now is the clock. Default time.Now.
	Now func() time.Time

	// Rand is the source of lease tokens. Default crypto/rand.Reader.
	Rand io.Reader

	// Sleep waits for d or until ctx is done. Default a timer.
	Sleep func(ctx context.Context, d time.Duration) error

	// PollInterval is how often a blocked MarkActive or AddHold retries.
	// Default 10 s.
	PollInterval time.Duration

	// WaitTimeout is how long a blocked MarkActive or AddHold waits before
	// returning an error wrapping ErrWaitTimeout. Default 10 min.
	WaitTimeout time.Duration

	// StaleAfter is how old a lease heartbeat, or the sentinel, must be before
	// another caller may take it over. Default 20 min.
	StaleAfter time.Duration

	// TablePollInterval is how often a caller waiting for the table to become
	// ACTIVE, or to finish deleting, checks again. Default 2 s.
	TablePollInterval time.Duration

	// MarkBound is how long an idle Observe takes a creator mark for a
	// creator still running: while one is younger than this, an idle
	// Observe records nothing; an older one is a creator that died, removed
	// with the rest. A creator that may run longer refreshes its mark
	// (RefreshMark) well within it. Readers pass their own bound to InUse
	// and IdleSince. Default 1 h.
	MarkBound time.Duration
}

// Client is the lease protocol over the lifecycle tables. It is safe for
// concurrent use.
type Client struct {
	o       Options
	mu      sync.Mutex
	randMu  sync.Mutex // serialises reads of Options.Rand
	clients map[string]API
	// id is this Client's own random id (clientID), written with every
	// lease it takes, so AdoptLease never takes another Client's lease for
	// its own. Empty until its first lease.
	id string
}

// New returns a Client.
func New(o Options) (*Client, error) {
	if o.DynamoDB == nil {
		return nil, errors.New("lifecycle: Options.DynamoDB is required")
	}
	if o.Owner == "" {
		o.Owner = "unknown"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Rand == nil {
		o.Rand = rand.Reader
	}
	if o.Sleep == nil {
		o.Sleep = sleep
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	if o.WaitTimeout <= 0 {
		o.WaitTimeout = DefaultWaitTimeout
	}
	if o.StaleAfter <= 0 {
		o.StaleAfter = DefaultStaleAfter
	}
	if o.TablePollInterval <= 0 {
		o.TablePollInterval = DefaultTablePollInterval
	}
	if o.MarkBound <= 0 {
		o.MarkBound = DefaultMarkBound
	}
	return &Client{o: o, clients: map[string]API{}}, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// api returns the client for region.
func (c *Client) api(region string) (API, error) {
	if region == "" {
		return nil, errors.New("lifecycle: empty region")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if a, ok := c.clients[region]; ok {
		return a, nil
	}
	a, err := c.o.DynamoDB(region)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: dynamodb client for %s: %w", region, err)
	}
	if a == nil {
		return nil, fmt.Errorf("lifecycle: no dynamodb client for %s", region)
	}
	c.clients[region] = a
	return a, nil
}

// RegionFor returns the region whose table holds scope's items: the control
// region for ScopeGlobal, and scope itself otherwise.
func (c *Client) RegionFor(scope string) (string, error) {
	if scope == ScopeGlobal {
		if c.o.ControlRegion == "" {
			return "", errors.New("lifecycle: scope global needs Options.ControlRegion")
		}
		return c.o.ControlRegion, nil
	}
	return scope, nil
}

// ItemKey returns the pk of a tool item.
func ItemKey(tool tags.Tool, scope string) string {
	return "tool#" + string(tool) + "#" + scope
}

func validate(tool tags.Tool, scope string) error {
	if tool == "" || strings.Contains(string(tool), "#") {
		return fmt.Errorf("lifecycle: invalid tool %q", tool)
	}
	if scope == "" || strings.Contains(scope, "#") {
		return fmt.Errorf("lifecycle: invalid scope %q", scope)
	}
	return nil
}

// target resolves tool and scope to a client, its region and the item's key.
func (c *Client) target(tool tags.Tool, scope string) (API, string, string, error) {
	if err := validate(tool, scope); err != nil {
		return nil, "", "", err
	}
	region, err := c.RegionFor(scope)
	if err != nil {
		return nil, "", "", err
	}
	a, err := c.api(region)
	if err != nil {
		return nil, "", "", err
	}
	return a, region, ItemKey(tool, scope), nil
}

// now is the Client's clock at the stored precision (milliseconds,
// timeLayout): every time a writer writes or compares is this, so a time
// read back equals the time written.
func (c *Client) now() time.Time { return c.o.Now().UTC().Truncate(time.Millisecond) }

// staleBefore is the heartbeat (or sentinel) time before which a lease is
// stale, as of now.
func (c *Client) staleBefore(now time.Time) string { return formatTime(now.Add(-c.o.StaleAfter)) }

// sentinelStaleCond is the condition that the closing sentinel (or an item
// with its key) is not a live closer's: sentinelStale's rule, as DynamoDB
// compares the stored strings (a comparison with a missing attribute, or one
// of another type, is false). It uses #since, :stale and :future
// (sentinelVals).
const sentinelStaleCond = "NOT (#since >= :stale AND #since <= :future)"

// sentinelVals are sentinelStaleCond's values, as of now.
func (c *Client) sentinelVals(now time.Time) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{":stale": s(c.staleBefore(now)), ":future": s(formatTime(now.Add(c.o.StaleAfter)))}
}

// sentinelStale is the one definition of a closing sentinel no live closer
// holds, shared by the Go side and sentinelStaleCond: a live one's since, as
// stored, is a string from now-StaleAfter to now+StaleAfter. One older was
// left by a closer that died (or stalled past StaleAfter); one with no since,
// one that is not a string, and one that is not a time (it sorts outside the
// window, or ages out of it) no live closer wrote. Each is stale, and any
// writer or closer may take it over.
func (c *Client) sentinelStale(item map[string]types.AttributeValue, now time.Time) bool {
	v, ok := item[attrSince].(*types.AttributeValueMemberS)
	return !ok || v.Value < c.staleBefore(now) || v.Value > formatTime(now.Add(c.o.StaleAfter))
}

// leaseLive is the one definition of a live lease, shared by every check
// here (leaseStale, InUse) and by the noLiveLease condition: its heartbeat,
// as stored, compares at or after now-staleAfter, as DynamoDB compares the
// two strings. A lease with no heartbeat is stale; so is one whose heartbeat
// is not a time and sorts before the bound (both sides compare the same
// strings, so they agree on any value).
func leaseLive(heartbeat string, now time.Time, staleAfter time.Duration) bool {
	return heartbeat != "" && heartbeat >= formatTime(now.Add(-staleAfter))
}

// leaseStale is leaseLive's negation for l, at the Client's StaleAfter.
func (c *Client) leaseStale(l *LeaseInfo, now time.Time) bool {
	return !leaseLive(l.stored(), now, c.o.StaleAfter)
}

// clientID is this Client's own random id, made with its first lease.
func (c *Client) clientID() (string, error) {
	c.mu.Lock()
	id := c.id
	c.mu.Unlock()
	if id != "" {
		return id, nil
	}
	// Made outside the lock (Rand has its own), then set if still unset.
	id, err := c.newToken()
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.id == "" {
		c.id = id
	}
	return c.id, nil
}

func (c *Client) newToken() (string, error) {
	b := make([]byte, 16)
	// Options.Rand need not be safe for concurrent use: every read of it
	// is serialised.
	c.randMu.Lock()
	_, err := io.ReadFull(c.o.Rand, b)
	c.randMu.Unlock()
	if err != nil {
		return "", fmt.Errorf("lifecycle: lease token: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}

// timeLayout is RFC 3339 at a fixed width (milliseconds, always UTC "Z"), so
// that DynamoDB's string comparison of two timestamps is chronological.
const timeLayout = "2006-01-02T15:04:05.000Z"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
