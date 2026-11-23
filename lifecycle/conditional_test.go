package lifecycle_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/spore-host/libs/lifecycle"
)

func (e *env) read(scope string) (lifecycle.Item, bool) {
	e.t.Helper()
	it, ok, err := e.c.Get(context.Background(), tool, scope)
	if err != nil {
		e.t.Fatal(err)
	}
	return it, ok
}

// TestMarkAndMarkDone: a mark carries a token and a time; MarkDone ends it
// (IDLE, idleSince now), but only the latest mark's: an earlier creator's
// done leaves a later creator's ACTIVE.
func TestMarkAndMarkDone(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, b := e.client("creator-a"), e.client("the creator b")
	ta, err := a.Mark(ctx, tool, lifecycle.ScopeGlobal)
	if err != nil {
		t.Fatal(err)
	}
	it, _ := e.read(lifecycle.ScopeGlobal)
	if it.State != lifecycle.StateActive || it.Mark == nil || it.Mark.Token != ta || !it.Mark.At.Equal(t0) || it.Epoch != 1 {
		t.Fatalf("after Mark: %+v", it)
	}
	e.advance(time.Minute)
	tb, err := b.Mark(ctx, tool, lifecycle.ScopeGlobal)
	if err != nil || tb == ta {
		t.Fatalf("second mark %q %v", tb, err)
	}
	if done, err := a.MarkDone(ctx, tool, lifecycle.ScopeGlobal, ta); done || err != nil {
		t.Errorf("an earlier mark's done: %v %v", done, err)
	}
	if it, _ := e.read(lifecycle.ScopeGlobal); it.State != lifecycle.StateActive {
		t.Errorf("a later creator's mark was ended: %+v", it)
	}
	e.advance(time.Minute)
	if done, err := b.MarkDone(ctx, tool, lifecycle.ScopeGlobal, tb); !done || err != nil {
		t.Errorf("the last mark's done: %v %v", done, err)
	}
	it, _ = e.read(lifecycle.ScopeGlobal)
	if it.State != lifecycle.StateIdle || it.Mark != nil || !it.IdleSince.Equal(t0.Add(2*time.Minute)) {
		t.Errorf("after the done: %+v", it)
	}
	// Done on an item that is gone, and MarkActive (no token to keep) also
	// writes a mark.
	if done, err := a.MarkDone(ctx, tool, region, "x"); done || err != nil {
		t.Errorf("no table: %v %v", done, err)
	}
	if err := a.MarkActive(ctx, tool, lifecycle.ScopeGlobal); err != nil {
		t.Fatal(err)
	}
	if it, _ := e.read(lifecycle.ScopeGlobal); it.Mark == nil {
		t.Errorf("MarkActive wrote no mark: %+v", it)
	}
	// A hold keeps it active: done does not idle it.
	tc, _ := a.Mark(ctx, tool, region)
	if err := a.AddHold(ctx, tool, region, "spawn"); err != nil {
		t.Fatal(err)
	}
	it, _ = e.read(region)
	if done, _ := a.MarkDone(ctx, tool, region, it.Mark.Token); done {
		t.Errorf("held, but idled (%s)", tc)
	}
}

// TestObserveClearsMarkAndIncomplete: an idle Observe removes the creator
// mark with ACTIVE; an active one clears a suspended teardown's mark.
func TestObserveClearsMarkAndIncomplete(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.c.Mark(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.c.Observe(ctx, tool, region, false); err != nil {
		t.Fatal(err)
	}
	if it, _ := e.read(region); it.Mark != nil || it.State != lifecycle.StateIdle {
		t.Errorf("after an idle observe: %+v", it)
	}
	l, err := e.c.Claim(ctx, tool, region, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Suspend(ctx); err != nil {
		t.Fatal(err)
	}
	if it, _ := e.read(region); !it.Incomplete || it.Lease != nil || it.State != lifecycle.StateIdle {
		t.Errorf("after Suspend: %+v", it)
	}
	if _, _, err := e.c.Observe(ctx, tool, region, true); err != nil {
		t.Fatal(err)
	}
	if it, _ := e.read(region); it.Incomplete {
		t.Errorf("activity left the incomplete mark: %+v", it)
	}
}

// TestSuspendDoesNotBlockCreators: a suspended teardown holds no lease, so a
// creator marks at once and clears the incomplete mark; the lease is done.
func TestSuspendDoesNotBlockCreators(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, _, err := e.c.Observe(ctx, tool, lifecycle.ScopeGlobal, false); err != nil {
		t.Fatal(err)
	}
	l, err := e.c.Claim(ctx, tool, lifecycle.ScopeGlobal, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Suspend(ctx); err != nil {
		t.Fatal(err)
	}
	if err := l.Step(ctx, "x"); !errors.Is(err, lifecycle.ErrLeaseLost) {
		t.Errorf("a step after Suspend: %v", err)
	}
	creator := e.client("a creator", func(o *lifecycle.Options) { o.WaitTimeout = time.Nanosecond })
	if err := creator.MarkActive(ctx, tool, lifecycle.ScopeGlobal); err != nil {
		t.Fatalf("a creator after Suspend: %v", err)
	}
	if it, _ := e.read(lifecycle.ScopeGlobal); it.Incomplete || it.State != lifecycle.StateActive {
		t.Errorf("after the creator: %+v", it)
	}
}

// TestSetIdleIfUnchanged: creates an absent item IDLE (and its table), turns
// an ACTIVE one IDLE only at the mark and epoch read, and refuses any change.
func TestSetIdleIfUnchanged(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	it, err := e.c.SetIdleIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, lifecycle.Expect{})
	if err != nil || it.State != lifecycle.StateIdle || !it.IdleSince.Equal(t0) {
		t.Fatalf("absent: %+v %v", it, err)
	}
	// Absent expected, but there: changed.
	if _, err := e.c.SetIdleIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, lifecycle.Expect{}); !errors.Is(err, lifecycle.ErrChanged) {
		t.Errorf("expected absent: %v", err)
	}
	if _, err := e.c.Mark(ctx, tool, lifecycle.ScopeGlobal); err != nil {
		t.Fatal(err)
	}
	judged, ok := e.read(lifecycle.ScopeGlobal)
	// A creator marks again after the judgement: refused.
	e.advance(time.Minute)
	if _, err := e.client("another creator").Mark(ctx, tool, lifecycle.ScopeGlobal); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.SetIdleIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, lifecycle.ExpectOf(judged, ok)); !errors.Is(err, lifecycle.ErrChanged) {
		t.Errorf("after a newer mark: %v", err)
	}
	if it, _ := e.read(lifecycle.ScopeGlobal); it.State != lifecycle.StateActive {
		t.Errorf("the creator's mark was erased: %+v", it)
	}
	now, ok := e.read(lifecycle.ScopeGlobal)
	if it, err := e.c.SetIdleIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, lifecycle.ExpectOf(now, ok)); err != nil || it.State != lifecycle.StateIdle || it.Mark != nil {
		t.Errorf("as read: %+v %v", it, err)
	}
	// A table that is closing.
	e2 := newEnv(t)
	if _, err := e2.c.Mark(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	it, ok = e2.read(region)
	putSentinel(t, e2, t0)
	if _, err := e2.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok)); !errors.Is(err, lifecycle.ErrTableClosing) {
		t.Errorf("closing: %v", err)
	}
	// A failure of the write itself.
	e3 := newEnv(t)
	e3.fakes[control].Err = func(op string, _ any) error {
		if op == "TransactWriteItems" {
			return errors.New("network down")
		}
		return nil
	}
	if _, err := e3.c.SetIdleIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, lifecycle.Expect{}); err == nil || errors.Is(err, lifecycle.ErrChanged) {
		t.Errorf("a failing write: %v", err)
	}
}

// TestClaimIfUnchanged: a claim only at the epoch, state, lease and mark
// read; the grace for IDLE; a stale lease taken over; a live one refused.
func TestClaimIfUnchanged(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.c.SetIdleIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, lifecycle.Expect{}); err != nil {
		t.Fatal(err)
	}
	judged, ok := e.read(lifecycle.ScopeGlobal)
	if _, err := e.c.ClaimIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, grace, lifecycle.ExpectOf(judged, ok)); !errors.Is(err, lifecycle.ErrNotClaimable) {
		t.Errorf("within the grace: %v", err)
	}
	// A creator marks after the judgement: refused, though grace 0.
	creator := e.client("a creator")
	tok, _ := creator.Mark(ctx, tool, lifecycle.ScopeGlobal)
	if _, err := e.c.ClaimIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, 0, lifecycle.ExpectOf(judged, ok)); !errors.Is(err, lifecycle.ErrNotClaimable) {
		t.Errorf("after a creator: %v", err)
	}
	if done, _ := creator.MarkDone(ctx, tool, lifecycle.ScopeGlobal, tok); !done {
		t.Fatal("done")
	}
	judged, ok = e.read(lifecycle.ScopeGlobal)
	l, err := e.c.ClaimIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, 0, lifecycle.ExpectOf(judged, ok))
	if err != nil {
		t.Fatal(err)
	}
	// Live: refused.
	held, ok := e.read(lifecycle.ScopeGlobal)
	if _, err := e.client("other one").ClaimIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, 0, lifecycle.ExpectOf(held, ok)); !errors.Is(err, lifecycle.ErrNotClaimable) {
		t.Errorf("a live lease: %v", err)
	}
	// Stale: taken over, the old holder fenced.
	e.advance(21 * time.Minute)
	l2, err := e.client("other one").ClaimIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, 0, lifecycle.ExpectOf(held, ok))
	if err != nil {
		t.Fatalf("stale takeover: %v", err)
	}
	if err := l.Step(ctx, "x"); !errors.Is(err, lifecycle.ErrLeaseLost) {
		t.Errorf("the old holder: %v", err)
	}
	if err := l2.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	// Absent, no table, ACTIVE, held, negative grace.
	if _, err := e.c.ClaimIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, 0, lifecycle.Expect{}); !errors.Is(err, lifecycle.ErrNotClaimable) {
		t.Errorf("absent: %v", err)
	}
	if _, err := newEnv(t).c.ClaimIfUnchanged(ctx, tool, region, 0, lifecycle.Expect{Exists: true}); !errors.Is(err, lifecycle.ErrNotClaimable) {
		t.Errorf("no table: %v", err)
	}
	if _, err := e.c.Mark(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	act, ok := e.read(region)
	if _, err := e.c.ClaimIfUnchanged(ctx, tool, region, 0, lifecycle.ExpectOf(act, ok)); !errors.Is(err, lifecycle.ErrNotClaimable) {
		t.Errorf("ACTIVE: %v", err)
	}
	if err := e.c.AddHold(ctx, tool, region, "spawn"); err != nil {
		t.Fatal(err)
	}
	h, ok := e.read(region)
	if _, err := e.c.ClaimIfUnchanged(ctx, tool, region, 0, lifecycle.ExpectOf(h, ok)); !errors.Is(err, lifecycle.ErrNotClaimable) {
		t.Errorf("held: %v", err)
	}
	if _, err := e.c.ClaimIfUnchanged(ctx, tool, region, -1, lifecycle.ExpectOf(h, ok)); err == nil {
		t.Error("negative grace")
	}
	// A read that fails, and a claim write that fails.
	e4 := newEnv(t)
	if _, err := e4.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.Expect{}); err != nil {
		t.Fatal(err)
	}
	it, ok := e4.read(region)
	e4.fakes[region].Err = func(op string, _ any) error {
		if op == "UpdateItem" {
			return errors.New("network down")
		}
		return nil
	}
	if _, err := e4.c.ClaimIfUnchanged(ctx, tool, region, 0, lifecycle.ExpectOf(it, ok)); err == nil || errors.Is(err, lifecycle.ErrNotClaimable) {
		t.Errorf("a failing claim: %v", err)
	}
	e4.fakes[region].Err = func(string, any) error { return errors.New("network down") }
	if _, err := e4.c.ClaimIfUnchanged(ctx, tool, region, 0, lifecycle.ExpectOf(it, ok)); err == nil || errors.Is(err, lifecycle.ErrNotClaimable) {
		t.Errorf("a failing read: %v", err)
	}
	if _, err := e4.c.MarkDone(ctx, tool, region, "x"); err == nil {
		t.Error("a failing done")
	}
}

// TestMarkDoneClosing: a done while the table closes.
func TestMarkDoneClosing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tok, err := e.c.Mark(ctx, tool, region)
	if err != nil {
		t.Fatal(err)
	}
	putSentinel(t, e, t0)
	if _, err := e.c.MarkDone(ctx, tool, region, tok); !errors.Is(err, lifecycle.ErrTableClosing) {
		t.Errorf("closing: %v", err)
	}
}
