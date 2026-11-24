package lifecycle_test

import (
	"context"
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/lifecycle/lifecycletest"
	"github.com/spore-host/libs/tags"
)

// stamped is an item as this package's writers leave it: State and Epoch
// stamped.
func stamped(st lifecycle.State, epoch int64) lifecycle.Item {
	return lifecycle.Item{Tool: tool, Scope: region, State: st, Epoch: epoch, StampState: st, StampEpoch: epoch}
}

// TestInUseDefinition: every clause of InUse, and its edges.
func TestInUseDefinition(t *testing.T) {
	now := t0
	with := func(it lifecycle.Item, f func(*lifecycle.Item)) lifecycle.Item { f(&it); return it }
	cases := []struct {
		name string
		it   lifecycle.Item
		want bool
	}{
		{"absent", lifecycle.Item{}, false},
		{"ACTIVE with nothing in it", stamped(lifecycle.StateActive, 1), false},
		{"a live lease", with(stamped(lifecycle.StateTeardown, 1), func(i *lifecycle.Item) {
			i.Lease = &lifecycle.LeaseInfo{Heartbeat: now.Add(-19 * time.Minute)}
		}), true},
		{"a stale lease", with(stamped(lifecycle.StateTeardown, 1), func(i *lifecycle.Item) {
			i.Lease = &lifecycle.LeaseInfo{Heartbeat: now.Add(-20 * time.Minute)}
		}), false},
		{"a hold", with(stamped(lifecycle.StateIdle, 1), func(i *lifecycle.Item) { i.Holds = []tags.Tool{"spawn"} }), true},
		{"a young mark", with(stamped(lifecycle.StateActive, 1), func(i *lifecycle.Item) {
			i.Marks = []lifecycle.MarkInfo{{Token: "a", At: now.Add(-bound + time.Second)}}
		}), true},
		{"a mark at the bound", with(stamped(lifecycle.StateActive, 1), func(i *lifecycle.Item) {
			i.Marks = []lifecycle.MarkInfo{{Token: "a", At: now.Add(-bound)}}
		}), false},
		{"a mark with no time", with(stamped(lifecycle.StateIdle, 1), func(i *lifecycle.Item) {
			i.Marks = []lifecycle.MarkInfo{{Token: "a"}}
		}), true},
		{"a young pulse", with(stamped(lifecycle.StateActive, 1), func(i *lifecycle.Item) { i.PulseAt = now.Add(-time.Minute) }), true},
		{"an old pulse", with(stamped(lifecycle.StateActive, 1), func(i *lifecycle.Item) { i.PulseAt = now.Add(-bound) }), false},
		{"an older client's ACTIVE (no stamps)", lifecycle.Item{State: lifecycle.StateActive, Epoch: 3}, true},
		{"an older client's activation since", with(stamped(lifecycle.StateActive, 1), func(i *lifecycle.Item) { i.Epoch = 2 }), true},
		{"an older client's active observe over IDLE", with(stamped(lifecycle.StateIdle, 1), func(i *lifecycle.Item) {
			i.State = lifecycle.StateActive
		}), true},
		{"an older client's IDLE", lifecycle.Item{State: lifecycle.StateIdle, IdleSince: now.Add(-time.Hour)}, false},
	}
	for _, c := range cases {
		if got := lifecycle.InUse(c.it, now, bound); got != c.want {
			t.Errorf("%s: InUse = %v, want %v", c.name, got, c.want)
		}
		if got := lifecycle.IdleSince(c.it, now, bound); c.want && !got.IsZero() {
			t.Errorf("%s: in use, but idle since %v", c.name, got)
		}
	}
}

// TestIdleSinceDefinition: the latest of the times, and the stored IdleSince
// only for an older client's write or an item with no times.
func TestIdleSinceDefinition(t *testing.T) {
	now := t0.Add(10 * time.Hour)
	at := func(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }
	base := stamped(lifecycle.StateActive, 2)
	cases := []struct {
		name string
		it   func(lifecycle.Item) lifecycle.Item
		want time.Time
	}{
		{"absent", func(lifecycle.Item) lifecycle.Item { return lifecycle.Item{} }, time.Time{}},
		{"last activity", func(i lifecycle.Item) lifecycle.Item { i.LastActivityAt = at(3); return i }, at(3)},
		{"a dead creator's mark", func(i lifecycle.Item) lifecycle.Item {
			i.LastActivityAt = at(1)
			i.Marks = []lifecycle.MarkInfo{{Token: "a", At: at(2)}, {Token: "b", At: at(4)}}
			return i
		}, at(5)},
		{"a pulse", func(i lifecycle.Item) lifecycle.Item { i.LastActivityAt, i.PulseAt = at(6), at(6); return i }, at(7)},
		{"a stale lease", func(i lifecycle.Item) lifecycle.Item {
			i.State, i.StampState = lifecycle.StateTeardown, lifecycle.StateTeardown
			i.LastActivityAt = at(1)
			i.Lease = &lifecycle.LeaseInfo{Heartbeat: at(8)}
			return i
		}, at(8).Add(lifecycle.DefaultStaleAfter)},
		{"our IDLE: the stored idleSince is not a time of activity", func(i lifecycle.Item) lifecycle.Item {
			i.State, i.StampState = lifecycle.StateIdle, lifecycle.StateIdle
			i.LastActivityAt, i.IdleSince = at(2), at(5)
			return i
		}, at(2)},
		{"an older client's idle observe since", func(lifecycle.Item) lifecycle.Item {
			i := stamped(lifecycle.StateActive, 2)
			i.State, i.LastActivityAt, i.IdleSince = lifecycle.StateIdle, at(2), at(5)
			return i
		}, at(5)},
		{"an older client's item, no times", func(lifecycle.Item) lifecycle.Item {
			return lifecycle.Item{State: lifecycle.StateIdle, IdleSince: at(4)}
		}, at(4)},
		{"in use", func(i lifecycle.Item) lifecycle.Item {
			i.Holds = []tags.Tool{"spawn"}
			i.LastActivityAt = at(1)
			return i
		}, time.Time{}},
	}
	for _, c := range cases {
		if got := lifecycle.IdleSince(c.it(base), now, bound); !got.Equal(c.want) {
			t.Errorf("%s: IdleSince = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestClientJudge: the Client's methods judge with its clock and StaleAfter.
func TestClientJudge(t *testing.T) {
	e := newEnv(t, func(o *lifecycle.Options) { o.StaleAfter = time.Minute })
	it := stamped(lifecycle.StateTeardown, 1)
	it.Lease = &lifecycle.LeaseInfo{Heartbeat: t0.Add(-2 * time.Minute)}
	if e.c.InUse(it, bound) || !e.c.IdleSince(it, bound).Equal(t0.Add(-time.Minute)) {
		t.Errorf("with a 1 min StaleAfter: %v %v", e.c.InUse(it, bound), e.c.IdleSince(it, bound))
	}
	if !lifecycle.InUse(it, t0, bound) {
		t.Error("the package default is 20 min")
	}
}

// TestReviewFinding1FailedDoneAgesOut: with no second write to fail, a done
// leaves nothing stuck; a done that fails leaves the mark, which the reader
// takes for a creator that died once it is older than the bound.
func TestReviewFinding1FailedDoneAgesOut(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tok, _ := e.c.Mark(ctx, tool, region)
	e.fake().Err = func(op string, _ any) error {
		if op == "UpdateItem" {
			return errors.New("network down")
		}
		return nil
	}
	if _, err := e.c.MarkDone(ctx, tool, region, tok); err == nil {
		t.Fatal("a failing done")
	}
	e.fake().Err = nil
	if it, _ := e.read(region); !lifecycle.InUse(it, e.clock(), bound) {
		t.Errorf("a creator that may still run: %+v", it)
	}
	e.advance(bound)
	it, ok := e.read(region)
	if lifecycle.InUse(it, e.clock(), bound) || !lifecycle.IdleSince(it, e.clock(), bound).Equal(t0.Add(bound)) {
		t.Errorf("past the bound: %+v", it)
	}
	// The backstop acts on it only as judged, which removes the dead mark.
	if got, err := e.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok)); err != nil || len(got.Marks) != 0 || got.State != lifecycle.StateIdle {
		t.Errorf("set idle as judged: %+v %v", got, err)
	}
	// RemoveHold the same: one write, then judged.
	if err := e.c.AddHold(ctx, tool, lifecycle.ScopeGlobal, "spawn"); err != nil {
		t.Fatal(err)
	}
	if err := e.c.RemoveHold(ctx, tool, lifecycle.ScopeGlobal, "spawn"); err != nil {
		t.Fatal(err)
	}
	if it, _ := e.read(lifecycle.ScopeGlobal); lifecycle.InUse(it, e.clock(), bound) {
		t.Errorf("after RemoveHold: %+v", it)
	}
}

// TestReviewFinding2OlderClients: an older client's activity, which records
// no time, is in use: on an item it made, after a newer write (its
// activation bumps the epoch past the stamp), and its active observe over an
// IDLE item. A backstop's idle Observe, as before, ends it.
func TestReviewFinding2OlderClients(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	pk := &types.AttributeValueMemberS{Value: "tool#lagotto#us-west-2"}
	put := func(m map[string]types.AttributeValue) {
		m["pk"] = pk
		if err := e.fake().PutRaw(lifecycle.TableName, m); err != nil {
			t.Fatal(err)
		}
	}
	S := func(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }
	N := func(v string) types.AttributeValue { return &types.AttributeValueMemberN{Value: v} }
	e.advance(2 * bound)
	for name, m := range map[string]map[string]types.AttributeValue{
		"made by an older client":      {"state": S("ACTIVE"), "epoch": N("1")},
		"an older activation since":    {"state": S("ACTIVE"), "epoch": N("2"), "stampState": S("ACTIVE"), "stampEpoch": N("1"), "lastActivityAt": S("2026-11-16T12:00:00.000Z")},
		"an older active observe":      {"state": S("ACTIVE"), "epoch": N("1"), "stampState": S("IDLE"), "stampEpoch": N("1"), "lastActivityAt": S("2026-11-16T12:00:00.000Z")},
		"an older release over a TEAR": {"state": S("ACTIVE"), "epoch": N("1"), "stampState": S("TEARDOWN"), "stampEpoch": N("1")},
	} {
		put(m)
		it, _ := e.read(region)
		if !lifecycle.InUse(it, e.clock(), bound) {
			t.Errorf("%s: not in use: %+v", name, it)
		}
	}
	if _, _, err := e.c.Observe(ctx, tool, region, false); err != nil {
		t.Fatal(err)
	}
	if it, _ := e.read(region); lifecycle.InUse(it, e.clock(), bound) || it.State != lifecycle.StateIdle {
		t.Errorf("after an idle observe: %+v", it)
	}
}

// TestReviewFinding3DoneDuringLock: a done while a lock is held removes the
// mark; after the unlock nothing is stuck: the reader judges the item idle
// since the done.
func TestReviewFinding3DoneDuringLock(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tok, _ := e.client("creator").Mark(ctx, tool, region)
	it, ok := e.read(region)
	l, err := e.c.LockIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok))
	if err != nil {
		t.Fatal(err)
	}
	e.advance(time.Minute)
	if done, err := e.client("creator").MarkDone(ctx, tool, region, tok); !done || err != nil {
		t.Fatalf("a done during the lock: %v %v", done, err)
	}
	if err := l.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
	it, _ = e.read(region)
	if lifecycle.InUse(it, e.clock(), bound) || !lifecycle.IdleSince(it, e.clock(), bound).Equal(t0.Add(time.Minute)) {
		t.Errorf("after the unlock: %+v", it)
	}
}

// TestReviewFinding5ReleaseAgesOut: a Release leaves the item ACTIVE with
// nothing in it; the reader judges it idle from the release, with no writer
// needed.
func TestReviewFinding5ReleaseAgesOut(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if _, err := e.c.SetIdleIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, lifecycle.Expect{}); err != nil {
		t.Fatal(err)
	}
	it, ok := e.read(lifecycle.ScopeGlobal)
	l, err := e.c.ClaimIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, 0, lifecycle.ExpectOf(it, ok))
	if err != nil {
		t.Fatal(err)
	}
	e.advance(time.Minute)
	if err := l.Release(ctx); err != nil {
		t.Fatal(err)
	}
	it, _ = e.read(lifecycle.ScopeGlobal)
	if it.State != lifecycle.StateActive || lifecycle.InUse(it, e.clock(), bound) || !lifecycle.IdleSince(it, e.clock(), bound).Equal(t0.Add(time.Minute)) {
		t.Errorf("after Release: %+v", it)
	}
}

// loseReply makes the next op on f apply, then fail as a dropped connection.
func loseReply(f *lifecycletest.Fake, op string) {
	f.Err = func(o string, in any) error {
		if o != op {
			return nil
		}
		f.Err = nil
		switch x := in.(type) {
		case *dynamodb.TransactWriteItemsInput:
			_, _ = f.TransactWriteItems(context.Background(), x)
		case *dynamodb.UpdateItemInput:
			_, _ = f.UpdateItem(context.Background(), x)
		}
		return errors.New("connection reset")
	}
}

// TestReviewFinding6LostMarkReply: a mark that committed though its answer
// was lost comes back with its token, so the creator can end it; one never
// ended ages out.
func TestReviewFinding6LostMarkReply(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	loseReply(e.fake(), "TransactWriteItems")
	tok, err := e.c.Mark(ctx, tool, region)
	if err == nil || tok == "" {
		t.Fatalf("a lost reply: %q %v", tok, err)
	}
	if it, _ := e.read(region); len(it.Marks) != 1 || it.Marks[0].Token != tok {
		t.Fatalf("the orphan: %+v", it)
	}
	if done, err := e.c.MarkDone(ctx, tool, region, tok); !done || err != nil {
		t.Errorf("ending it with the returned token: %v %v", done, err)
	}
	// Not ended: in use for the bound, then not.
	loseReply(e.fake(), "TransactWriteItems")
	_, _ = e.c.Mark(ctx, tool, region)
	e.advance(bound)
	if it, _ := e.read(region); lifecycle.InUse(it, e.clock(), bound) {
		t.Errorf("an orphan past the bound: %+v", it)
	}
}

// TestReviewFinding4AdoptOwnOnly: AdoptLease takes only a lease this very
// Client took: not another Client's with the same owner name, and nothing
// for a Client with no owner.
func TestReviewFinding4AdoptOwnOnly(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if _, err := e.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.Expect{}); err != nil {
		t.Fatal(err)
	}
	it, ok := e.read(region)
	loseReply(e.fake(), "UpdateItem")
	if _, err := e.c.ClaimIfUnchanged(ctx, tool, region, 0, lifecycle.ExpectOf(it, ok)); err == nil {
		t.Fatal("a lost claim reply")
	}
	twin := e.client("tester", func(o *lifecycle.Options) { o.Rand = rand.New(rand.NewSource(99)) })
	if _, err := twin.AdoptLease(ctx, tool, region); !errors.Is(err, lifecycle.ErrLeaseLost) {
		t.Errorf("a Client with the same owner adopted: %v", err)
	}
	fresh := e.client("tester", func(o *lifecycle.Options) { o.Rand = rand.New(rand.NewSource(98)) })
	if _, err := fresh.AdoptLease(ctx, tool, region); !errors.Is(err, lifecycle.ErrLeaseLost) {
		t.Errorf("a Client with no lease yet adopted: %v", err)
	}
	mine, err := e.c.AdoptLease(ctx, tool, region)
	if err != nil {
		t.Fatalf("its own: %v", err)
	}
	if err := mine.Release(ctx); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"", "unknown"} {
		if _, err := e.client(owner).AdoptLease(ctx, tool, region); !errors.Is(err, lifecycle.ErrLeaseLost) {
			t.Errorf("owner %q adopted: %v", owner, err)
		}
	}
}

// TestReviewFinding7ClaimIdleStaleLease: an IDLE item left with a stale
// lease (a lock whose holder died) is claimable as judged, after its grace.
func TestReviewFinding7ClaimIdleStaleLease(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if _, err := e.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.Expect{}); err != nil {
		t.Fatal(err)
	}
	it, ok := e.read(region)
	if _, err := e.client("dies").LockIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok)); err != nil {
		t.Fatal(err)
	}
	it, ok = e.read(region)
	if _, err := e.c.ClaimIfUnchanged(ctx, tool, region, 0, lifecycle.ExpectOf(it, ok)); !errors.Is(err, lifecycle.ErrNotClaimable) {
		t.Errorf("a live lock: %v", err)
	}
	e.advance(21 * time.Minute)
	// The item's grace still applies.
	if _, err := e.c.ClaimIfUnchanged(ctx, tool, region, grace, lifecycle.ExpectOf(it, ok)); !errors.Is(err, lifecycle.ErrNotClaimable) {
		t.Errorf("within the grace: %v", err)
	}
	l, err := e.c.ClaimIfUnchanged(ctx, tool, region, 0, lifecycle.ExpectOf(it, ok))
	if err != nil {
		t.Fatalf("IDLE with a stale lease: %v", err)
	}
	if it, _ := e.read(region); it.State != lifecycle.StateTeardown || it.Lease == nil || it.Lease.Token != l.Token() {
		t.Errorf("after the takeover: %+v", it)
	}
}

// TestReviewFinding9Applied: a set-idle whose write applied but whose read
// back failed says so (ErrApplied), not that it failed.
func TestReviewFinding9Applied(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	it, ok := e.read(region)
	e.fake().Err = func(op string, _ any) error {
		if op == "GetItem" {
			return errors.New("network down")
		}
		return nil
	}
	_, err := e.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok))
	if !errors.Is(err, lifecycle.ErrApplied) || errors.Is(err, lifecycle.ErrChanged) {
		t.Errorf("read back failing: %v", err)
	}
	e.fake().Err = nil
	if it, _ := e.read(region); it.State != lifecycle.StateIdle {
		t.Errorf("the write: %+v", it)
	}
}

// TestReviewFinding10MarkTimeAtCommit: a mark that waited out a live lease
// carries the time it committed, not the time it began.
func TestReviewFinding10MarkTimeAtCommit(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	l := e.idleAndClaim(t)
	e.onSleep = func(n int) {
		if n == 3 {
			if err := l.Release(ctx); err != nil {
				t.Error(err)
			}
		}
	}
	tok, err := e.client("creator").Mark(ctx, tool, region)
	if err != nil {
		t.Fatal(err)
	}
	it, _ := e.read(region)
	if len(it.Marks) != 1 || it.Marks[0].Token != tok || !it.Marks[0].At.Equal(e.clock()) || it.Marks[0].At.Equal(t0) {
		t.Errorf("the mark's time: %+v at %v", it.Marks, e.clock())
	}
}

// oneToken gives one token's worth of randomness, then fails.
type oneToken struct{ n int }

func (r *oneToken) Read(b []byte) (int, error) {
	if r.n > 0 {
		return 0, errors.New("no entropy")
	}
	r.n++
	for i := range b {
		b[i] = 1
	}
	return len(b), nil
}

// TestClientIDNeedsRandomness: a claim or lock whose Client id cannot be made
// takes nothing.
func TestClientIDNeedsRandomness(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if _, _, err := e.c.Observe(ctx, tool, region, false); err != nil {
		t.Fatal(err)
	}
	it, ok := e.read(region)
	short := func() *lifecycle.Client {
		return e.client("short", func(o *lifecycle.Options) { o.Rand = &oneToken{} })
	}
	if _, err := short().Claim(ctx, tool, region, 0); err == nil {
		t.Error("Claim")
	}
	if _, err := short().ClaimIfUnchanged(ctx, tool, region, 0, lifecycle.ExpectOf(it, ok)); err == nil {
		t.Error("ClaimIfUnchanged")
	}
	if _, err := short().LockIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok)); err == nil {
		t.Error("LockIfUnchanged")
	}
	if it, _ := e.read(region); it.Lease != nil {
		t.Errorf("a lease was taken: %+v", it)
	}
}

// TestExpectCoversActivityStamps: an active Observe or a Release between the
// judgement and the write is a change, though it bumps no epoch.
func TestExpectCoversActivityStamps(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if _, _, err := e.c.Observe(ctx, tool, region, true); err != nil {
		t.Fatal(err)
	}
	e.advance(time.Minute)
	it, ok := e.read(region)
	if _, _, err := e.c.Observe(ctx, tool, region, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok)); !errors.Is(err, lifecycle.ErrChanged) {
		t.Errorf("an active observe since the judgement: %v", err)
	}
	it, ok = e.read(region)
	if _, err := e.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok)); err != nil {
		t.Errorf("as judged: %v", err)
	}
}
