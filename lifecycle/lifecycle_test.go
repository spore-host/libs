package lifecycle_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/tags"
	"github.com/spore-host/libs/tags/dynamodbtags"
)

var ctx = context.Background()

func TestNewRequiresDynamoDB(t *testing.T) {
	if _, err := lifecycle.New(lifecycle.Options{}); err == nil {
		t.Fatal("New without DynamoDB succeeded")
	}
}

func TestMarkActiveCreatesTaggedTable(t *testing.T) {
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	creates := e.fake().CreateTableInputs()
	if len(creates) != 1 {
		t.Fatalf("CreateTable called %d times, want 1", len(creates))
	}
	in := creates[0]
	if aws.ToString(in.TableName) != lifecycle.TableName || in.BillingMode != types.BillingModePayPerRequest {
		t.Errorf("table %q billing %q", aws.ToString(in.TableName), in.BillingMode)
	}
	if len(in.KeySchema) != 1 || aws.ToString(in.KeySchema[0].AttributeName) != "pk" {
		t.Errorf("key schema %+v", in.KeySchema)
	}
	tg := dynamodbtags.ToMap(in.Tags)
	if !tags.IsManagedBy(tg, tags.ToolShared) || tg[tags.Component] != "lifecycle" || tg[tags.Scope] != region {
		t.Errorf("table tags %v", tg)
	}
	it := e.item(t)
	if it.State != lifecycle.StateActive || it.Epoch != 1 || it.Tool != tool || it.Scope != region {
		t.Errorf("item after MarkActive = %+v", it)
	}
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	if it := e.item(t); it.Epoch != 2 {
		t.Errorf("epoch after two MarkActive = %d, want 2", it.Epoch)
	}
	if n := len(e.fake().CreateTableInputs()); n != 1 {
		t.Errorf("second MarkActive created the table again (%d creates)", n)
	}
}

// The lifecycle table must never carry spore-host:role. role=service is
// reserved for the live spore.host service, and backstops' roles have an
// explicit Deny on resources tagged with it (lagotto#333), which DynamoDB
// enforces through aws:ResourceTag: a role-tagged lease table would lock out
// every backstop that must use it.
func TestTableTagsHaveNoRole(t *testing.T) {
	e := newEnv(t)
	tg := e.c.TableTags(region, t0)
	if v, ok := tg[tags.Role]; ok {
		t.Errorf("TableTags carries %s=%q; the lease table must have no role tag", tags.Role, v)
	}
	if tags.IsService(tg) {
		t.Error("TableTags marks the lease table as the spore.host service")
	}
	want := map[string]string{
		tags.ManagedBy: string(tags.ToolShared), tags.Component: "lifecycle", tags.Scope: region,
		tags.Created: tags.FormatCreated(t0), tags.Schema: tags.SchemaVersion, tags.Owner: "tester",
	}
	if len(tg) != len(want) {
		t.Errorf("TableTags = %v, want exactly %v", tg, want)
	}
	for k, v := range want {
		if tg[k] != v {
			t.Errorf("TableTags[%s] = %q, want %q", k, tg[k], v)
		}
	}
	// And what CreateTable actually sends.
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	sent := dynamodbtags.ToMap(e.fake().CreateTableInputs()[0].Tags)
	if _, ok := sent[tags.Role]; ok {
		t.Errorf("CreateTable sent a role tag: %v", sent)
	}
}

func TestMarkActiveWaitsForCreatingTable(t *testing.T) {
	e := newEnv(t)
	e.fake().CreateDelay = 3
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	if len(e.sleeps) == 0 {
		t.Error("MarkActive did not wait for a CREATING table")
	}
	for _, d := range e.sleeps {
		if d != lifecycle.DefaultTablePollInterval {
			t.Errorf("slept %s, want the table poll interval", d)
		}
	}
	if e.item(t).State != lifecycle.StateActive {
		t.Error("item not written once the table was ACTIVE")
	}
}

func TestMarkActiveWaitsOutDeletingTable(t *testing.T) {
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	e.fake().SetStatus(lifecycle.TableName, types.TableStatusDeleting, 2)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	if n := len(e.fake().CreateTableInputs()); n != 2 {
		t.Errorf("table created %d times, want 2 (again after the delete)", n)
	}
	if it := e.item(t); it.Epoch != 1 {
		t.Errorf("epoch on the recreated table = %d, want 1", it.Epoch)
	}
}

func TestMarkActiveToleratesLosingTheCreateRace(t *testing.T) {
	e := newEnv(t)
	f := e.fake()
	won := false
	f.Hook = func(c context.Context, op string) {
		if op == "CreateTable" && !won {
			won = true
			f.Hook = nil
			// Another caller's CreateTable lands first.
			if err := e.client("other").MarkActive(c, tool, region); err != nil {
				t.Errorf("other MarkActive: %v", err)
			}
		}
	}
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatalf("MarkActive after losing the create race: %v", err)
	}
	if it := e.item(t); it.Epoch != 2 {
		t.Errorf("epoch = %d, want 2", it.Epoch)
	}
}

func TestMarkActiveRemovesIdleSince(t *testing.T) {
	e := newEnv(t)
	if _, _, err := e.c.Observe(ctx, tool, region, false); err != nil {
		t.Fatal(err)
	}
	if e.item(t).IdleSince.IsZero() {
		t.Fatal("Observe(idle) did not set idleSince")
	}
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	it := e.item(t)
	if !it.IdleSince.IsZero() || it.State != lifecycle.StateActive || it.Epoch != 1 {
		t.Errorf("after MarkActive: %+v", it)
	}
}

func TestMarkActiveTimesOutBehindLiveLease(t *testing.T) {
	e := newEnv(t)
	e.idleAndClaim(t)
	e.sleeps = nil
	err := e.client("creator").MarkActive(ctx, tool, region)
	var we *lifecycle.WaitError
	if !errors.As(err, &we) || !errors.Is(err, lifecycle.ErrWaitTimeout) {
		t.Fatalf("MarkActive behind a live lease = %v, want a WaitError", err)
	}
	if we.Blocker != "lease" || we.Owner != "tester" || we.Waited < lifecycle.DefaultWaitTimeout {
		t.Errorf("WaitError = %+v", we)
	}
	if !strings.Contains(err.Error(), "held by tester") {
		t.Errorf("error %q does not name the holder", err)
	}
	if n := len(e.sleeps); n != int(lifecycle.DefaultWaitTimeout/lifecycle.DefaultPollInterval) {
		t.Errorf("polled %d times, want %d", n, lifecycle.DefaultWaitTimeout/lifecycle.DefaultPollInterval)
	}
	for _, d := range e.sleeps {
		if d != lifecycle.DefaultPollInterval {
			t.Errorf("poll interval %s", d)
		}
	}
	if e.item(t).State != lifecycle.StateTeardown {
		t.Error("a timed-out MarkActive changed the item")
	}
}

func TestMarkActiveProceedsOnceLeaseIsReleased(t *testing.T) {
	e := newEnv(t)
	l := e.idleAndClaim(t)
	e.sleeps = nil
	e.onSleep = func(n int) {
		if n == 3 {
			if err := l.Release(ctx); err != nil {
				t.Errorf("Release: %v", err)
			}
		}
	}
	if err := e.client("creator").MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	if len(e.sleeps) != 3 {
		t.Errorf("MarkActive polled %d times, want 3", len(e.sleeps))
	}
	it := e.item(t)
	if it.State != lifecycle.StateActive || it.Lease != nil || it.Epoch != 2 {
		t.Errorf("after MarkActive: %+v", it)
	}
}

func TestMarkActiveProceedsOnceTeardownFinishes(t *testing.T) {
	e := newEnv(t)
	l := e.idleAndClaim(t)
	e.sleeps = nil
	e.onSleep = func(n int) {
		if n == 1 {
			if err := l.Finish(ctx); err != nil {
				t.Errorf("Finish: %v", err)
			}
		}
	}
	if err := e.client("creator").MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	if it := e.item(t); it.State != lifecycle.StateActive || it.Epoch != 1 {
		t.Errorf("after MarkActive on a finished item: %+v", it)
	}
}

func TestStaleLeaseIsTakenOverAfter20Minutes(t *testing.T) {
	e := newEnv(t, func(o *lifecycle.Options) { o.WaitTimeout = time.Minute })
	l := e.idleAndClaim(t)

	// Exactly 20 min old is not yet stale: "older than 20 min".
	e.advance(lifecycle.DefaultStaleAfter)
	if err := blockedOnce(e).MarkActive(ctx, tool, region); !errors.Is(err, errBlocked) {
		t.Fatalf("MarkActive at exactly 20 min = %v, want it to wait", err)
	}

	e.advance(time.Millisecond)
	e.sleeps = nil
	if err := e.client("creator").MarkActive(ctx, tool, region); err != nil {
		t.Fatalf("MarkActive over a stale lease: %v", err)
	}
	if len(e.sleeps) != 0 {
		t.Errorf("MarkActive waited %d polls on a stale lease", len(e.sleeps))
	}
	it := e.item(t)
	if it.Lease != nil || it.State != lifecycle.StateActive {
		t.Errorf("stale lease not removed: %+v", it)
	}
	// The old holder is fenced off.
	if err := l.Step(ctx, "delete something"); !errors.Is(err, lifecycle.ErrLeaseLost) {
		t.Errorf("Step after takeover = %v, want ErrLeaseLost", err)
	}
	if err := l.Finish(ctx); !errors.Is(err, lifecycle.ErrLeaseLost) {
		t.Errorf("Finish after takeover = %v, want ErrLeaseLost", err)
	}
	if _, ok, _ := e.c.Get(ctx, tool, region); !ok {
		t.Error("a fenced Finish deleted the item")
	}
}

func TestHeartbeatKeepsLeaseLive(t *testing.T) {
	e := newEnv(t)
	l := e.idleAndClaim(t)
	e.advance(15 * time.Minute)
	if err := l.Step(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	e.advance(15 * time.Minute) // 30 min since the claim, 15 since the heartbeat
	if err := blockedOnce(e).MarkActive(ctx, tool, region); !errors.Is(err, errBlocked) {
		t.Fatalf("MarkActive with a 15-min-old heartbeat = %v, want it to wait", err)
	}
	it := e.item(t)
	if it.Lease == nil || it.Lease.Step != "one" || !it.Lease.Heartbeat.Equal(t0.Add(grace+15*time.Minute)) {
		t.Errorf("lease after Step = %+v", it.Lease)
	}
}

func putSentinel(t *testing.T, e *env, since time.Time) {
	t.Helper()
	err := e.fake().PutRaw(lifecycle.TableName, map[string]types.AttributeValue{
		"pk": sAttr(lifecycle.SentinelKey), "token": sAttr("closer"), "owner": sAttr("closer"), "since": sAttr(ts(since)),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMarkActiveBlockedBySentinel(t *testing.T) {
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	putSentinel(t, e, e.clock())
	short := e.client("creator", func(o *lifecycle.Options) { o.WaitTimeout = 30 * time.Second })
	err := short.MarkActive(ctx, tool, region)
	var we *lifecycle.WaitError
	if !errors.As(err, &we) || we.Blocker != "sentinel" {
		t.Fatalf("MarkActive with the sentinel present = %v, want a sentinel WaitError", err)
	}
	if e.item(t).Epoch != 1 {
		t.Error("MarkActive wrote past the sentinel")
	}

	// The closer gives up (items remain) and removes its sentinel while this
	// creator waits.
	e.sleeps = nil
	e.onSleep = func(n int) {
		if n == 2 {
			if _, err := e.fake().DeleteItem(ctx, deleteSentinel()); err != nil {
				t.Errorf("remove sentinel: %v", err)
			}
		}
	}
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatalf("MarkActive once the sentinel is gone: %v", err)
	}
	if len(e.sleeps) != 2 || e.item(t).Epoch != 2 {
		t.Errorf("polled %d times, epoch %d", len(e.sleeps), e.item(t).Epoch)
	}
}

func TestMarkActiveClearsAbandonedSentinel(t *testing.T) {
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	putSentinel(t, e, e.clock().Add(-lifecycle.DefaultStaleAfter-time.Second))
	e.sleeps = nil
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	if len(e.sleeps) != 0 {
		t.Errorf("waited %d polls on an abandoned sentinel", len(e.sleeps))
	}
	if e.fake().Item(lifecycle.TableName, lifecycle.SentinelKey) != nil {
		t.Error("abandoned sentinel still present")
	}
}

func TestMarkActiveStopsOnCancelledContext(t *testing.T) {
	e := newEnv(t)
	e.idleAndClaim(t)
	c, cancel := context.WithCancel(ctx)
	cancel()
	if err := e.client("creator").MarkActive(c, tool, region); !errors.Is(err, context.Canceled) {
		t.Errorf("MarkActive on a cancelled context = %v", err)
	}
}

func TestMarkActiveReportsAPIErrors(t *testing.T) {
	e := newEnv(t)
	boom := errors.New("boom")
	e.fake().Err = func(op string, _ any) error {
		if op == "TransactWriteItems" {
			return boom
		}
		return nil
	}
	if err := e.c.MarkActive(ctx, tool, region); !errors.Is(err, boom) {
		t.Errorf("MarkActive = %v, want boom", err)
	}
	e.fake().Err = func(op string, _ any) error {
		if op == "DescribeTable" {
			return boom
		}
		if op == "TransactWriteItems" {
			return &types.ResourceNotFoundException{}
		}
		return nil
	}
	if err := e.c.MarkActive(ctx, tool, region); !errors.Is(err, boom) {
		t.Errorf("MarkActive with a failing DescribeTable = %v, want boom", err)
	}
	fresh := newEnv(t)
	fresh.fake().Err = func(op string, _ any) error {
		if op == "CreateTable" {
			return boom
		}
		return nil
	}
	if err := fresh.c.MarkActive(ctx, tool, region); !errors.Is(err, boom) {
		t.Errorf("MarkActive with a failing CreateTable = %v, want boom", err)
	}
}

func TestMarkActiveConflictIsRetried(t *testing.T) {
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	n := 0
	e.fake().Err = func(op string, _ any) error {
		if op == "TransactWriteItems" && n < 2 {
			n++
			return &types.TransactionCanceledException{CancellationReasons: []types.CancellationReason{
				{Code: aws.String("None")}, {Code: aws.String("TransactionConflict")},
			}}
		}
		return nil
	}
	e.sleeps = nil
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	if len(e.sleeps) != 2 {
		t.Errorf("retried %d times after conflicts, want 2", len(e.sleeps))
	}
}

func TestMarkActiveTableThatKeepsVanishing(t *testing.T) {
	e := newEnv(t, func(o *lifecycle.Options) { o.WaitTimeout = time.Minute })
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	e.fake().Err = func(op string, _ any) error {
		if op == "TransactWriteItems" {
			return &types.ResourceNotFoundException{}
		}
		return nil
	}
	err := e.c.MarkActive(ctx, tool, region)
	var we *lifecycle.WaitError
	if !errors.As(err, &we) || we.Blocker != "table" {
		t.Errorf("MarkActive against a vanishing table = %v", err)
	}
}

func TestEnsureTableRejectsUnusableStatus(t *testing.T) {
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	e.fake().SetStatus(lifecycle.TableName, types.TableStatusInaccessibleEncryptionCredentials, 0)
	e.fake().Err = func(op string, _ any) error {
		if op == "TransactWriteItems" {
			return &types.ResourceNotFoundException{}
		}
		return nil
	}
	if err := e.c.MarkActive(ctx, tool, region); err == nil || !strings.Contains(err.Error(), "INACCESSIBLE") {
		t.Errorf("MarkActive on an unusable table = %v", err)
	}
	e.fake().Err = nil
	e.fake().SetStatus(lifecycle.TableName, types.TableStatusCreating, 1000)
	short := e.client("x", func(o *lifecycle.Options) { o.WaitTimeout = 5 * time.Second })
	err := short.MarkActive(ctx, tool, region)
	var we *lifecycle.WaitError
	if !errors.As(err, &we) || we.Blocker != "table CREATING" {
		t.Errorf("MarkActive on a table stuck CREATING = %v", err)
	}
}

func TestValidation(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		tool  tags.Tool
		scope string
	}{{"", region}, {"a#b", region}, {tool, ""}, {tool, "us#1"}} {
		if err := e.c.MarkActive(ctx, c.tool, c.scope); err == nil {
			t.Errorf("MarkActive(%q, %q) succeeded", c.tool, c.scope)
		}
	}
	if err := e.c.AddHold(ctx, tool, region, ""); err == nil {
		t.Error("AddHold with an empty holder succeeded")
	}
	if err := e.c.RemoveHold(ctx, tool, region, "x#y"); err == nil {
		t.Error("RemoveHold with an invalid holder succeeded")
	}
	if _, err := e.c.Claim(ctx, tool, region, -time.Second); err == nil {
		t.Error("Claim with negative grace succeeded")
	}
	if _, err := e.c.CloseTableIfEmpty(ctx, ""); err == nil {
		t.Error("CloseTableIfEmpty on an empty region succeeded")
	}
	noControl := e.client("x", func(o *lifecycle.Options) { o.ControlRegion = "" })
	if err := noControl.MarkActive(ctx, tool, lifecycle.ScopeGlobal); err == nil {
		t.Error("global scope without a control region succeeded")
	}
	broken := e.client("x", func(o *lifecycle.Options) {
		o.DynamoDB = func(string) (lifecycle.API, error) { return nil, errors.New("no creds") }
	})
	if _, _, err := broken.Observe(ctx, tool, region, true); err == nil || !strings.Contains(err.Error(), "no creds") {
		t.Errorf("client factory error = %v", err)
	}
	nilClient := e.client("x", func(o *lifecycle.Options) {
		o.DynamoDB = func(string) (lifecycle.API, error) { return nil, nil }
	})
	if _, err := nilClient.Claim(ctx, tool, region, grace); err == nil {
		t.Error("nil client accepted")
	}
}

func TestGlobalScopeUsesControlRegion(t *testing.T) {
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, lifecycle.ScopeGlobal); err != nil {
		t.Fatal(err)
	}
	if e.fakes[control].Item(lifecycle.TableName, lifecycle.ItemKey(tool, lifecycle.ScopeGlobal)) == nil {
		t.Error("global item not in the control region")
	}
	if e.fakes[region].Status(lifecycle.TableName) != "" {
		t.Error("global scope touched another region")
	}
	if r, _ := e.c.RegionFor(region); r != region {
		t.Errorf("RegionFor(%s) = %s", region, r)
	}
}

func TestObserveIdleKeepsFirstIdleSince(t *testing.T) {
	e := newEnv(t)
	it, idle, err := e.c.Observe(ctx, tool, region, false)
	if err != nil {
		t.Fatal(err)
	}
	if it.State != lifecycle.StateIdle || !it.IdleSince.Equal(t0) || idle != 0 || it.Epoch != 0 {
		t.Fatalf("first idle Observe = %+v, %s", it, idle)
	}
	e.advance(3 * time.Hour)
	it, idle, err = e.c.Observe(ctx, tool, region, false)
	if err != nil {
		t.Fatal(err)
	}
	if !it.IdleSince.Equal(t0) || idle != 3*time.Hour {
		t.Errorf("second idle Observe = %+v, %s; want idleSince kept", it, idle)
	}
	if n := e.fake().Calls("TransactWriteItems"); n != 1 {
		t.Errorf("an unchanged idle observation wrote again (%d transactions)", n)
	}
}

func TestObserveActiveClearsIdle(t *testing.T) {
	e := newEnv(t)
	if _, _, err := e.c.Observe(ctx, tool, region, false); err != nil {
		t.Fatal(err)
	}
	e.advance(time.Hour)
	it, idle, err := e.c.Observe(ctx, tool, region, true)
	if err != nil {
		t.Fatal(err)
	}
	if it.State != lifecycle.StateActive || !it.IdleSince.IsZero() || idle != 0 {
		t.Errorf("active Observe = %+v, %s", it, idle)
	}
	// An unchanged active observation writes nothing.
	before := e.fake().Calls("TransactWriteItems")
	if _, _, err := e.c.Observe(ctx, tool, region, true); err != nil {
		t.Fatal(err)
	}
	if e.fake().Calls("TransactWriteItems") != before {
		t.Error("an unchanged active observation wrote")
	}
	// Idle again: the grace clock starts over.
	e.advance(time.Hour)
	it, _, _ = e.c.Observe(ctx, tool, region, false)
	if !it.IdleSince.Equal(t0.Add(2 * time.Hour)) {
		t.Errorf("idleSince after active then idle = %s, want a fresh one", it.IdleSince)
	}
}

func TestObserveDoesNotOverwriteAConcurrentMarkActive(t *testing.T) {
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	f := e.fake()
	f.Hook = func(c context.Context, op string) {
		if op == "TransactWriteItems" {
			f.Hook = nil
			// Lands between Observe's read and its write.
			if err := e.client("creator").MarkActive(c, tool, region); err != nil {
				t.Errorf("MarkActive: %v", err)
			}
		}
	}
	it, idle, err := e.c.Observe(ctx, tool, region, false)
	if err != nil {
		t.Fatal(err)
	}
	if it.State != lifecycle.StateActive || !it.IdleSince.IsZero() || it.Epoch != 2 || idle != 0 {
		t.Errorf("Observe(idle) raced by MarkActive = %+v, %s; want the creator's ACTIVE kept", it, idle)
	}
}

func TestObserveDoesNotOverwriteAConcurrentHold(t *testing.T) {
	e := newEnv(t)
	if _, _, err := e.c.Observe(ctx, tool, region, true); err != nil {
		t.Fatal(err)
	}
	f := e.fake()
	f.Hook = func(c context.Context, op string) {
		if op == "TransactWriteItems" {
			f.Hook = nil
			if err := e.client("calque").AddHold(c, tool, region, tags.ToolCalque); err != nil {
				t.Errorf("AddHold: %v", err)
			}
		}
	}
	it, _, err := e.c.Observe(ctx, tool, region, false)
	if err != nil {
		t.Fatal(err)
	}
	if it.State != lifecycle.StateActive || len(it.Holds) != 1 {
		t.Errorf("Observe(idle) raced by AddHold = %+v", it)
	}
}

// A hold always arrives with an epoch bump (AddHold is MarkActive plus the
// hold), so Observe's epoch condition already catches a racing AddHold. This
// isolates its holds condition: a hold appears with the epoch unchanged.
func TestObserveConditionHolds(t *testing.T) {
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	f := e.fake()
	f.Hook = func(_ context.Context, op string) {
		if op == "TransactWriteItems" {
			f.Hook = nil
			raw := f.Item(lifecycle.TableName, lifecycle.ItemKey(tool, region))
			raw["holds"] = &types.AttributeValueMemberSS{Value: []string{"calque"}}
			if err := f.PutRaw(lifecycle.TableName, raw); err != nil {
				t.Fatal(err)
			}
		}
	}
	it, _, err := e.c.Observe(ctx, tool, region, false)
	if err != nil {
		t.Fatal(err)
	}
	if it.State != lifecycle.StateActive || !it.IdleSince.IsZero() {
		t.Errorf("Observe(idle) wrote over a hold: %+v", it)
	}
}

func TestHoldsCountAsActivity(t *testing.T) {
	e := newEnv(t)
	if err := e.c.AddHold(ctx, tool, region, tags.ToolCalque); err != nil {
		t.Fatal(err)
	}
	if err := e.c.AddHold(ctx, tool, region, tags.ToolSpawn); err != nil {
		t.Fatal(err)
	}
	it, idle, err := e.c.Observe(ctx, tool, region, false)
	if err != nil {
		t.Fatal(err)
	}
	if it.State != lifecycle.StateActive || idle != 0 || len(it.Holds) != 2 || it.Holds[0] != tags.ToolCalque || it.Epoch != 2 {
		t.Fatalf("idle Observe with holds = %+v", it)
	}
	// A hold also blocks Claim directly, whatever the state says.
	raw := e.fake().Item(lifecycle.TableName, lifecycle.ItemKey(tool, region))
	raw["state"] = sAttr("IDLE")
	raw["idleSince"] = sAttr(ts(t0.Add(-48 * time.Hour)))
	if err := e.fake().PutRaw(lifecycle.TableName, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Claim(ctx, tool, region, grace); !errors.Is(err, lifecycle.ErrNotClaimable) {
		t.Errorf("Claim with holds = %v", err)
	}

	for _, h := range []tags.Tool{tags.ToolCalque, tags.ToolSpawn, tags.ToolSpawn} {
		if err := e.c.RemoveHold(ctx, tool, region, h); err != nil {
			t.Fatal(err)
		}
	}
	it, _, _ = e.c.Get(ctx, tool, region)
	if len(it.Holds) != 0 {
		t.Errorf("holds after removing all = %v", it.Holds)
	}
	if _, err := e.c.Claim(ctx, tool, region, grace); err != nil {
		t.Errorf("Claim once the holds are gone = %v", err)
	}
}

func TestRemoveHoldWithoutItemOrTable(t *testing.T) {
	e := newEnv(t)
	if err := e.c.RemoveHold(ctx, tool, region, tags.ToolCalque); err != nil {
		t.Errorf("RemoveHold with no table = %v", err)
	}
	if err := e.c.MarkActive(ctx, tags.ToolSpawn, region); err != nil {
		t.Fatal(err)
	}
	if err := e.c.RemoveHold(ctx, tool, region, tags.ToolCalque); err != nil {
		t.Errorf("RemoveHold with no item = %v", err)
	}
	if _, ok, _ := e.c.Get(ctx, tool, region); ok {
		t.Error("RemoveHold created an item")
	}
	boom := errors.New("boom")
	e.fake().Err = func(string, any) error { return boom }
	if err := e.c.RemoveHold(ctx, tool, region, tags.ToolCalque); !errors.Is(err, boom) {
		t.Errorf("RemoveHold error = %v", err)
	}
}

func TestObserveLeavesLiveTeardownAlone(t *testing.T) {
	e := newEnv(t)
	l := e.idleAndClaim(t)
	it, idle, err := e.c.Observe(ctx, tool, region, false)
	if err != nil {
		t.Fatal(err)
	}
	if it.State != lifecycle.StateTeardown || it.Lease == nil || it.Lease.Token != l.Token() || idle != grace {
		t.Errorf("idle Observe during teardown = %+v, %s", it, idle)
	}
	if err := l.Step(ctx, "x"); err != nil {
		t.Errorf("Step after an idle Observe = %v", err)
	}
}

func TestObserveActiveBreaksTheLease(t *testing.T) {
	e := newEnv(t)
	l := e.idleAndClaim(t)
	it, _, err := e.c.Observe(ctx, tool, region, true)
	if err != nil {
		t.Fatal(err)
	}
	if it.State != lifecycle.StateActive || it.Lease != nil {
		t.Errorf("active Observe during teardown = %+v", it)
	}
	if err := l.Step(ctx, "x"); !errors.Is(err, lifecycle.ErrLeaseLost) {
		t.Errorf("Step after activity was observed = %v, want ErrLeaseLost", err)
	}
}

func TestObserveRecoversAbandonedTeardown(t *testing.T) {
	e := newEnv(t)
	l := e.idleAndClaim(t)
	e.advance(lifecycle.DefaultStaleAfter + time.Second)
	it, _, err := e.c.Observe(ctx, tool, region, false)
	if err != nil {
		t.Fatal(err)
	}
	if it.State != lifecycle.StateIdle || it.Lease != nil || !it.IdleSince.Equal(t0) {
		t.Fatalf("idle Observe of an abandoned teardown = %+v", it)
	}
	l2, err := e.client("backstop-2").Claim(ctx, tool, region, grace)
	if err != nil {
		t.Fatalf("re-claim of an abandoned teardown: %v", err)
	}
	if err := l.Step(ctx, "x"); !errors.Is(err, lifecycle.ErrLeaseLost) {
		t.Errorf("old holder's Step = %v, want ErrLeaseLost", err)
	}
	if err := l2.Step(ctx, "x"); err != nil {
		t.Errorf("new holder's Step = %v", err)
	}
}

func TestObserveWhileClosing(t *testing.T) {
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tags.ToolSpawn, region); err != nil {
		t.Fatal(err)
	}
	putSentinel(t, e, e.clock())
	if _, _, err := e.c.Observe(ctx, tool, region, true); !errors.Is(err, lifecycle.ErrTableClosing) {
		t.Errorf("Observe while closing = %v, want ErrTableClosing", err)
	}
	if _, ok, _ := e.c.Get(ctx, tool, region); ok {
		t.Error("Observe created an item past the sentinel")
	}
}

func TestObserveErrors(t *testing.T) {
	e := newEnv(t)
	if _, _, err := e.c.Observe(ctx, "", region, true); err == nil {
		t.Error("invalid tool accepted")
	}
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	for _, op := range []string{"GetItem", "TransactWriteItems"} {
		e.fake().Err = func(o string, _ any) error {
			if o == op {
				return boom
			}
			return nil
		}
		if _, _, err := e.c.Observe(ctx, tool, region, false); !errors.Is(err, boom) {
			t.Errorf("Observe with %s failing = %v", op, err)
		}
	}
	e.fake().Err = func(o string, _ any) error {
		if o == "TransactWriteItems" {
			return &types.TransactionCanceledException{CancellationReasons: []types.CancellationReason{{Code: aws.String("None")}, {Code: aws.String("TransactionConflict")}}}
		}
		return nil
	}
	if _, _, err := e.c.Observe(ctx, tool, region, false); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Errorf("Observe on a conflict = %v", err)
	}
	// The read after the write fails.
	n := 0
	e.fake().Err = func(o string, _ any) error {
		if o == "GetItem" {
			if n++; n == 2 {
				return boom
			}
		}
		return nil
	}
	if _, _, err := e.c.Observe(ctx, tool, region, true); !errors.Is(err, boom) {
		t.Errorf("Observe with the re-read failing = %v", err)
	}
}

func TestClaimNeedsTheFullGrace(t *testing.T) {
	e := newEnv(t)
	if _, _, err := e.c.Observe(ctx, tool, region, false); err != nil {
		t.Fatal(err)
	}
	e.advance(grace - time.Millisecond)
	_, err := e.c.Claim(ctx, tool, region, grace)
	var nce *lifecycle.NotClaimableError
	if !errors.As(err, &nce) || !strings.Contains(err.Error(), "idle for") {
		t.Fatalf("Claim before the grace = %v", err)
	}
	e.advance(time.Millisecond)
	l, err := e.c.Claim(ctx, tool, region, grace)
	if err != nil {
		t.Fatalf("Claim at the grace = %v", err)
	}
	it := e.item(t)
	if it.State != lifecycle.StateTeardown || it.Lease == nil || it.Lease.Token != l.Token() ||
		it.Lease.Owner != "tester" || !it.Lease.Heartbeat.Equal(e.clock()) || it.Lease.Step != "" {
		t.Errorf("item after Claim = %+v lease %+v", it, it.Lease)
	}
	if l.Tool() != tool || l.Scope() != region || len(l.Token()) != 32 {
		t.Errorf("lease = %s %s %q", l.Tool(), l.Scope(), l.Token())
	}
}

func TestClaimRefusals(t *testing.T) {
	e := newEnv(t)
	if _, err := e.c.Claim(ctx, tool, region, grace); !errors.Is(err, lifecycle.ErrNotClaimable) || !strings.Contains(err.Error(), "no lifecycle table") {
		t.Errorf("Claim with no table = %v", err)
	}
	if err := e.c.MarkActive(ctx, tags.ToolSpawn, region); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Claim(ctx, tool, region, grace); !errors.Is(err, lifecycle.ErrNotClaimable) || !strings.Contains(err.Error(), "no lifecycle item") {
		t.Errorf("Claim with no item = %v", err)
	}
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Claim(ctx, tool, region, 0); !errors.Is(err, lifecycle.ErrNotClaimable) || !strings.Contains(err.Error(), "ACTIVE") {
		t.Errorf("Claim while ACTIVE = %v", err)
	}
	if _, _, err := e.c.Observe(ctx, tool, region, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Claim(ctx, tool, region, 0); err != nil {
		t.Fatalf("Claim with zero grace = %v", err)
	}
	if _, err := e.client("other").Claim(ctx, tool, region, 0); !errors.Is(err, lifecycle.ErrNotClaimable) || !strings.Contains(err.Error(), "lease held by tester") {
		t.Errorf("second Claim = %v", err)
	}
	boom := errors.New("boom")
	e.fake().Err = func(string, any) error { return boom }
	if _, err := e.c.Claim(ctx, tool, region, 0); !errors.Is(err, boom) {
		t.Errorf("Claim with GetItem failing = %v", err)
	}
	e.fake().Err = func(op string, _ any) error {
		if op == "UpdateItem" {
			return boom
		}
		return nil
	}
	e.advance(time.Hour) // let the lease go stale so the read-side checks pass
	if _, err := e.c.Claim(ctx, tool, region, 0); err == nil {
		t.Error("Claim on a stale TEARDOWN succeeded; it must be observed idle first")
	}
	if _, err := e.c.Claim(ctx, "", region, 0); err == nil {
		t.Error("Claim with an invalid tool succeeded")
	}
}

func TestClaimRefusesRandSourceFailure(t *testing.T) {
	e := newEnv(t, func(o *lifecycle.Options) { o.Rand = strings.NewReader("short") })
	if _, _, err := e.c.Observe(ctx, tool, region, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Claim(ctx, tool, region, 0); err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("Claim with a failing random source = %v", err)
	}
	if _, err := e.c.CloseTableIfEmpty(ctx, region); err == nil {
		t.Error("CloseTableIfEmpty with a failing random source succeeded")
	}
}

// The next three tests change the item between Claim's read and its write,
// each in a way only one part of its condition catches.

func claimRacedBy(t *testing.T, change func(e *env, raw map[string]types.AttributeValue)) error {
	t.Helper()
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.c.Observe(ctx, tool, region, false); err != nil {
		t.Fatal(err)
	}
	e.advance(grace)
	f := e.fake()
	f.Hook = func(_ context.Context, op string) {
		if op == "UpdateItem" {
			f.Hook = nil
			raw := f.Item(lifecycle.TableName, lifecycle.ItemKey(tool, region))
			change(e, raw)
			if err := f.PutRaw(lifecycle.TableName, raw); err != nil {
				t.Fatal(err)
			}
		}
	}
	_, err := e.c.Claim(ctx, tool, region, grace)
	if err == nil {
		return nil
	}
	if !errors.Is(err, lifecycle.ErrNotClaimable) || !strings.Contains(err.Error(), "changed while claiming") {
		t.Fatalf("raced Claim = %v", err)
	}
	return err
}

func TestClaimConditionEpoch(t *testing.T) {
	// A creator activated the tool (epoch 2) and the item was set back to
	// IDLE with the old idleSince intact.
	if claimRacedBy(t, func(_ *env, raw map[string]types.AttributeValue) {
		raw["epoch"] = &types.AttributeValueMemberN{Value: "2"}
	}) == nil {
		t.Error("Claim succeeded although the epoch changed")
	}
}

func TestClaimConditionIdleSince(t *testing.T) {
	// The item was torn down and recreated (epoch back to 1) and observed
	// idle just now: same epoch, but the grace has not passed.
	if claimRacedBy(t, func(e *env, raw map[string]types.AttributeValue) {
		raw["idleSince"] = sAttr(ts(e.clock()))
	}) == nil {
		t.Error("Claim succeeded although idleSince is fresh")
	}
}

func TestClaimConditionHolds(t *testing.T) {
	if claimRacedBy(t, func(_ *env, raw map[string]types.AttributeValue) {
		raw["holds"] = &types.AttributeValueMemberSS{Value: []string{"calque"}}
	}) == nil {
		t.Error("Claim succeeded although a hold appeared")
	}
}

func TestClaimConditionLease(t *testing.T) {
	if claimRacedBy(t, func(e *env, raw map[string]types.AttributeValue) {
		raw["lease"] = &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
			"token": sAttr("other"), "owner": sAttr("other"), "heartbeat": sAttr(ts(e.clock())), "step": sAttr(""),
		}}
	}) == nil {
		t.Error("Claim succeeded although another lease is live")
	}
}

func TestClaimTakesOverStaleLeaseOnIdleItem(t *testing.T) {
	err := claimRacedBy(t, func(e *env, raw map[string]types.AttributeValue) {
		raw["lease"] = &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
			"token": sAttr("dead"), "owner": sAttr("dead"), "heartbeat": sAttr(ts(e.clock().Add(-time.Hour))), "step": sAttr("x"),
		}}
	})
	if err != nil {
		t.Errorf("Claim over a stale lease on an IDLE item = %v", err)
	}
}

func TestLeaseLostOnTokenMismatch(t *testing.T) {
	e := newEnv(t)
	l := e.idleAndClaim(t)
	raw := e.fake().Item(lifecycle.TableName, lifecycle.ItemKey(tool, region))
	raw["lease"].(*types.AttributeValueMemberM).Value["token"] = sAttr("someone-else")
	if err := e.fake().PutRaw(lifecycle.TableName, raw); err != nil {
		t.Fatal(err)
	}
	if err := l.Step(ctx, "delete role"); !errors.Is(err, lifecycle.ErrLeaseLost) {
		t.Fatalf("Step with a foreign token = %v, want ErrLeaseLost", err)
	}
	if it := e.item(t); it.Lease.Step != "" {
		t.Error("a fenced Step wrote")
	}
	before := e.fake().Calls("UpdateItem")
	if err := l.Release(ctx); !errors.Is(err, lifecycle.ErrLeaseLost) {
		t.Errorf("Release after loss = %v", err)
	}
	if e.fake().Calls("UpdateItem") != before {
		t.Error("a lost lease still called DynamoDB")
	}
}

func TestLeaseRelease(t *testing.T) {
	e := newEnv(t)
	l := e.idleAndClaim(t)
	if err := l.Release(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t)
	if it.State != lifecycle.StateActive || it.Lease != nil || !it.IdleSince.IsZero() {
		t.Errorf("after Release: %+v", it)
	}
	if err := l.Step(ctx, "x"); !errors.Is(err, lifecycle.ErrLeaseLost) {
		t.Errorf("Step after Release = %v", err)
	}
}

func TestLeaseFinish(t *testing.T) {
	e := newEnv(t)
	l := e.idleAndClaim(t)
	if err := l.Step(ctx, "delete queue"); err != nil {
		t.Fatal(err)
	}
	if err := l.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e.c.Get(ctx, tool, region); ok {
		t.Error("Finish left the item")
	}
	if err := l.Finish(ctx); !errors.Is(err, lifecycle.ErrLeaseLost) {
		t.Errorf("second Finish = %v", err)
	}
}

func TestLeaseOtherErrorsAreNotLoss(t *testing.T) {
	e := newEnv(t)
	l := e.idleAndClaim(t)
	boom := errors.New("throttled")
	e.fake().Err = func(string, any) error { return boom }
	if err := l.Step(ctx, "x"); !errors.Is(err, boom) || errors.Is(err, lifecycle.ErrLeaseLost) {
		t.Errorf("Step on a throttle = %v", err)
	}
	e.fake().Err = nil
	if err := l.Step(ctx, "x"); err != nil {
		t.Errorf("Step after a transient error = %v", err)
	}
	// The table vanished: the lease is certainly gone.
	e.fake().Err = func(string, any) error { return &types.ResourceNotFoundException{} }
	if err := l.Step(ctx, "x"); !errors.Is(err, lifecycle.ErrLeaseLost) {
		t.Errorf("Step on a missing table = %v", err)
	}
}

func TestCloseTableIfEmpty(t *testing.T) {
	e := newEnv(t)
	if gone, err := e.c.CloseTableIfEmpty(ctx, region); err != nil || !gone {
		t.Errorf("close with no table = %v, %v", gone, err)
	}
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	if err := e.c.MarkActive(ctx, tags.ToolSpawn, region); err != nil {
		t.Fatal(err)
	}
	e.fake().ScanPage = 1 // exercise pagination
	gone, err := e.c.CloseTableIfEmpty(ctx, region)
	if err != nil || gone {
		t.Fatalf("close with items = %v, %v", gone, err)
	}
	if e.fake().Item(lifecycle.TableName, lifecycle.SentinelKey) != nil {
		t.Error("sentinel left behind after a refused close")
	}
	for _, tl := range []tags.Tool{tool, tags.ToolSpawn} {
		if _, _, err := e.c.Observe(ctx, tl, region, false); err != nil {
			t.Fatal(err)
		}
	}
	e.advance(grace)
	for _, tl := range []tags.Tool{tool, tags.ToolSpawn} {
		l, err := e.c.Claim(ctx, tl, region, grace)
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Finish(ctx); err != nil {
			t.Fatal(err)
		}
	}
	gone, err = e.c.CloseTableIfEmpty(ctx, region)
	if err != nil || !gone {
		t.Fatalf("close when empty = %v, %v", gone, err)
	}
	d := e.fake().Deletions()
	if len(d) != 1 || len(d[0].Keys) != 1 || d[0].Keys[0] != lifecycle.SentinelKey {
		t.Errorf("deletions = %+v, want one with only the sentinel", d)
	}
}

func TestCloseTableIfEmptyDefersToAnotherCloser(t *testing.T) {
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	putSentinel(t, e, e.clock())
	if gone, err := e.c.CloseTableIfEmpty(ctx, region); err != nil || gone {
		t.Errorf("close with a fresh foreign sentinel = %v, %v", gone, err)
	}
	if v := e.fake().Item(lifecycle.TableName, lifecycle.SentinelKey); v == nil {
		t.Error("removed another closer's sentinel")
	}
	// Once that sentinel is abandoned, this closer takes over (and here finds
	// an item, so it backs off and cleans up).
	e.advance(lifecycle.DefaultStaleAfter + time.Second)
	if gone, err := e.c.CloseTableIfEmpty(ctx, region); err != nil || gone {
		t.Errorf("close over a stale sentinel with items = %v, %v", gone, err)
	}
	if e.fake().Item(lifecycle.TableName, lifecycle.SentinelKey) != nil {
		t.Error("stale sentinel not replaced and removed")
	}
}

func TestCloseTableIfEmptyMidTransition(t *testing.T) {
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	e.fake().SetStatus(lifecycle.TableName, types.TableStatusCreating, 5)
	if gone, err := e.c.CloseTableIfEmpty(ctx, region); err != nil || gone {
		t.Errorf("close on a CREATING table = %v, %v", gone, err)
	}
}

func TestCloseTableIfEmptyErrors(t *testing.T) {
	boom := errors.New("boom")
	for _, op := range []string{"PutItem", "Scan", "DeleteTable"} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t)
			if _, _, err := e.c.Observe(ctx, tool, region, true); err != nil {
				t.Fatal(err)
			}
			l := e.idleAndClaim(t)
			if err := l.Finish(ctx); err != nil {
				t.Fatal(err)
			}
			e.fake().Err = func(o string, _ any) error {
				if o == op {
					return boom
				}
				return nil
			}
			gone, err := e.c.CloseTableIfEmpty(ctx, region)
			if !errors.Is(err, boom) || gone {
				t.Errorf("close with %s failing = %v, %v", op, gone, err)
			}
			if e.fake().Item(lifecycle.TableName, lifecycle.SentinelKey) != nil {
				t.Error("sentinel left behind after a failed close")
			}
		})
	}
	t.Run("sentinel removal fails too", func(t *testing.T) {
		e := newEnv(t)
		if err := e.c.MarkActive(ctx, tool, region); err != nil {
			t.Fatal(err)
		}
		e.fake().Err = func(o string, _ any) error {
			if o == "Scan" || o == "DeleteItem" {
				return boom
			}
			return nil
		}
		if _, err := e.c.CloseTableIfEmpty(ctx, region); err == nil || !strings.Contains(err.Error(), "sentinel") {
			t.Errorf("close with scan and cleanup failing = %v", err)
		}
		e.fake().Err = func(o string, _ any) error {
			if o == "DeleteItem" {
				return boom
			}
			return nil
		}
		e.advance(lifecycle.DefaultStaleAfter + time.Second)
		if _, err := e.c.CloseTableIfEmpty(ctx, region); err == nil || !strings.Contains(err.Error(), "sentinel") {
			t.Errorf("close with items and cleanup failing = %v", err)
		}
	})
	t.Run("table vanished before DeleteTable", func(t *testing.T) {
		e := newEnv(t)
		if _, _, err := e.c.Observe(ctx, tool, region, false); err != nil {
			t.Fatal(err)
		}
		e.advance(grace)
		l, _ := e.c.Claim(ctx, tool, region, grace)
		_ = l.Finish(ctx)
		e.fake().Err = func(o string, _ any) error {
			if o == "DeleteTable" {
				return &types.ResourceNotFoundException{}
			}
			return nil
		}
		if gone, err := e.c.CloseTableIfEmpty(ctx, region); err != nil || !gone {
			t.Errorf("close when DeleteTable finds nothing = %v, %v", gone, err)
		}
	})
}

func TestGet(t *testing.T) {
	e := newEnv(t)
	if _, ok, err := e.c.Get(ctx, tool, region); ok || err != nil {
		t.Errorf("Get with no table = %v, %v", ok, err)
	}
	if _, _, err := e.c.Get(ctx, "", region); err == nil {
		t.Error("Get with an invalid tool succeeded")
	}
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	e.fake().Err = func(string, any) error { return boom }
	if _, _, err := e.c.Get(ctx, tool, region); !errors.Is(err, boom) {
		t.Errorf("Get error = %v", err)
	}
}

func TestItemIdleFor(t *testing.T) {
	it := lifecycle.Item{State: lifecycle.StateIdle, IdleSince: t0}
	if d := it.IdleFor(t0.Add(-time.Hour)); d != 0 {
		t.Errorf("IdleFor before idleSince = %s", d)
	}
	it.State = lifecycle.StateActive
	if d := it.IdleFor(t0.Add(time.Hour)); d != 0 {
		t.Errorf("IdleFor while ACTIVE = %s", d)
	}
}

func TestDefaultSleepHonoursContext(t *testing.T) {
	// Real clock and timer: the defaults.
	f := newEnv(t).fake()
	c, err := lifecycle.New(lifecycle.Options{
		DynamoDB:    func(string) (lifecycle.API, error) { return f, nil },
		WaitTimeout: time.Hour, PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	other, _ := lifecycle.New(lifecycle.Options{DynamoDB: func(string) (lifecycle.API, error) { return f, nil }})
	if _, _, err := other.Observe(ctx, tool, region, false); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Claim(ctx, tool, region, 0); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := c.MarkActive(cctx, tool, region); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("MarkActive with the default sleep = %v, want the context's deadline", err)
	}
}

func TestWaitErrorMessage(t *testing.T) {
	e := &lifecycle.WaitError{Tool: tool, Scope: region, Waited: time.Minute, Blocker: "sentinel"}
	if got := e.Error(); !strings.Contains(got, "sentinel") || strings.Contains(got, "held by") {
		t.Errorf("WaitError = %q", got)
	}
}

func deleteSentinel() *dynamodb.DeleteItemInput {
	return &dynamodb.DeleteItemInput{TableName: aws.String(lifecycle.TableName), Key: map[string]types.AttributeValue{"pk": sAttr(lifecycle.SentinelKey)}}
}

var errBlocked = errors.New("would wait")

// blockedOnce is a creator whose first wait fails with errBlocked, without
// moving the clock: it tells whether a call is blocked right now.
func blockedOnce(e *env) *lifecycle.Client {
	return e.client("creator", func(o *lifecycle.Options) {
		o.Sleep = func(context.Context, time.Duration) error { return errBlocked }
	})
}
