package lifecycle_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"

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

func tokens(it lifecycle.Item) []string {
	var out []string
	for _, m := range it.Marks {
		out = append(out, m.Token)
	}
	return out
}

// TestMarksAreASet: overlapping creators each hold a mark; the item stays
// ACTIVE until the last one is done, in any order, and only then goes IDLE,
// idle from that moment.
func TestMarksAreASet(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, b := e.client("creator-a"), e.client("the creator b")
	ta, err := a.Mark(ctx, tool, lifecycle.ScopeGlobal)
	if err != nil {
		t.Fatal(err)
	}
	it, _ := e.read(lifecycle.ScopeGlobal)
	if it.State != lifecycle.StateActive || len(it.Marks) != 1 || it.Marks[0].Token != ta || !it.Marks[0].At.Equal(t0) || it.Epoch != 1 ||
		!it.ActiveAt.Equal(t0) {
		t.Fatalf("after Mark: %+v", it)
	}
	e.advance(time.Minute)
	tb, err := b.Mark(ctx, tool, lifecycle.ScopeGlobal)
	if err != nil || tb == ta {
		t.Fatalf("second mark %q %v", tb, err)
	}
	// The later creator finishes first: the earlier one still runs.
	if done, err := b.MarkDone(ctx, tool, lifecycle.ScopeGlobal, tb); done || err != nil {
		t.Errorf("a done with another creator running: %v %v", done, err)
	}
	if it, _ := e.read(lifecycle.ScopeGlobal); it.State != lifecycle.StateActive || len(it.Marks) != 1 || it.Marks[0].Token != ta {
		t.Errorf("a creator still running lost its mark: %+v", it)
	}
	e.advance(time.Minute)
	if done, err := a.MarkDone(ctx, tool, lifecycle.ScopeGlobal, ta); !done || err != nil {
		t.Errorf("the last done: %v %v", done, err)
	}
	it, _ = e.read(lifecycle.ScopeGlobal)
	if it.State != lifecycle.StateIdle || len(it.Marks) != 0 || !it.IdleSince.Equal(t0.Add(2*time.Minute)) {
		t.Errorf("after the last done: %+v", it)
	}
	// A done twice, or on an item or table that is gone, is not an error.
	if done, err := a.MarkDone(ctx, tool, lifecycle.ScopeGlobal, ta); done || err != nil {
		t.Errorf("a second done: %v %v", done, err)
	}
	if done, err := a.MarkDone(ctx, tool, region, "x"); done || err != nil {
		t.Errorf("no table: %v %v", done, err)
	}
}

// TestHoldsAreInUse: a hold keeps an item in use like a mark; an item
// activated only by AddHold goes IDLE when the hold is removed, an earlier
// creator's done does not idle what a hold keeps, and MarkActive's activity
// keeps it ACTIVE (only a backstop's idle Observe ends that).
func TestHoldsAreInUse(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.c.AddHold(ctx, tool, lifecycle.ScopeGlobal, "spawn"); err != nil {
		t.Fatal(err)
	}
	if it, _ := e.read(lifecycle.ScopeGlobal); it.State != lifecycle.StateActive || len(it.Marks) != 0 || len(it.Holds) != 1 {
		t.Fatalf("after AddHold: %+v", it)
	}
	e.advance(time.Minute)
	if err := e.c.RemoveHold(ctx, tool, lifecycle.ScopeGlobal, "spawn"); err != nil {
		t.Fatal(err)
	}
	if it, _ := e.read(lifecycle.ScopeGlobal); it.State != lifecycle.StateIdle || !it.IdleSince.Equal(t0.Add(time.Minute)) {
		t.Errorf("a hold-only item after RemoveHold: %+v", it)
	}
	// A creator, then a hold: the creator's done leaves the hold's ACTIVE.
	ta, _ := e.c.Mark(ctx, tool, region)
	if err := e.c.AddHold(ctx, tool, region, "spawn"); err != nil {
		t.Fatal(err)
	}
	if done, _ := e.c.MarkDone(ctx, tool, region, ta); done {
		t.Error("a done idled what a hold keeps")
	}
	if it, _ := e.read(region); it.State != lifecycle.StateActive {
		t.Errorf("held: %+v", it)
	}
	if err := e.c.RemoveHold(ctx, tool, region, "spawn"); err != nil {
		t.Fatal(err)
	}
	if it, _ := e.read(region); it.State != lifecycle.StateIdle {
		t.Errorf("after the hold: %+v", it)
	}
	// MarkActive's activity: RemoveHold leaves it ACTIVE.
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	if err := e.c.AddHold(ctx, tool, region, "spawn"); err != nil {
		t.Fatal(err)
	}
	if err := e.c.RemoveHold(ctx, tool, region, "spawn"); err != nil {
		t.Fatal(err)
	}
	it, _ := e.read(region)
	if it.State != lifecycle.StateActive || len(it.Marks) != 1 || it.Marks[0].Token != "*" || !it.Marks[0].At.Equal(it.ActiveAt) {
		t.Errorf("MarkActive's activity after RemoveHold: %+v", it)
	}
	// A backstop's idle Observe clears every mark.
	if _, _, err := e.c.Observe(ctx, tool, region, false); err != nil {
		t.Fatal(err)
	}
	if it, _ := e.read(region); it.State != lifecycle.StateIdle || len(it.Marks) != 0 {
		t.Errorf("after an idle observe: %+v", it)
	}
	// Removing a hold from a gone item or table succeeds.
	if err := newEnv(t).c.RemoveHold(ctx, tool, region, "spawn"); err != nil {
		t.Errorf("no table: %v", err)
	}
}

// TestIncompleteCleared: a suspended teardown's mark is cleared by activity
// (an active Observe on an otherwise unchanged ACTIVE item too), by a Claim
// and by a Release.
func TestIncompleteCleared(t *testing.T) {
	ctx := context.Background()
	suspended := func(t *testing.T) *env {
		e := newEnv(t)
		if _, _, err := e.c.Observe(ctx, tool, region, false); err != nil {
			t.Fatal(err)
		}
		l, err := e.c.Claim(ctx, tool, region, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Suspend(ctx); err != nil {
			t.Fatal(err)
		}
		if it, _ := e.read(region); !it.Incomplete || it.Lease != nil || it.State != lifecycle.StateIdle {
			t.Fatalf("after Suspend: %+v", it)
		}
		return e
	}
	e := suspended(t)
	if _, _, err := e.c.Observe(ctx, tool, region, true); err != nil {
		t.Fatal(err)
	}
	if it, _ := e.read(region); it.Incomplete {
		t.Errorf("activity left it: %+v", it)
	}
	e = suspended(t)
	l, err := e.c.Claim(ctx, tool, region, 0)
	if err != nil {
		t.Fatal(err)
	}
	if it, _ := e.read(region); it.Incomplete {
		t.Errorf("a claim left it: %+v", it)
	}
	if err := l.Release(ctx); err != nil {
		t.Fatal(err)
	}
	// An active Observe on an ACTIVE item that is incomplete writes.
	e = suspended(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	if it, _ := e.read(region); it.Incomplete {
		t.Errorf("MarkActive left it: %+v", it)
	}
}

// TestSuspendDoesNotBlockCreators: a suspended teardown holds no lease, so a
// creator marks at once; the lease is done.
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
}

// TestSetIdleIfUnchanged: creates an absent item IDLE (and its table), turns
// an ACTIVE one IDLE only at the marks and epoch read, never removes a live
// lease, and refuses any change.
func TestSetIdleIfUnchanged(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	it, err := e.c.SetIdleIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, lifecycle.Expect{})
	if err != nil || it.State != lifecycle.StateIdle || !it.IdleSince.Equal(t0) {
		t.Fatalf("absent: %+v %v", it, err)
	}
	if _, err := e.c.SetIdleIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, lifecycle.Expect{}); !errors.Is(err, lifecycle.ErrChanged) {
		t.Errorf("expected absent: %v", err)
	}
	if _, err := e.c.Mark(ctx, tool, lifecycle.ScopeGlobal); err != nil {
		t.Fatal(err)
	}
	judged, ok := e.read(lifecycle.ScopeGlobal)
	e.advance(time.Minute)
	tb, err := e.client("another creator").Mark(ctx, tool, lifecycle.ScopeGlobal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.SetIdleIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, lifecycle.ExpectOf(judged, ok)); !errors.Is(err, lifecycle.ErrChanged) {
		t.Errorf("after a newer mark: %v", err)
	}
	// A creator's done between the read and the write changes the set too.
	now, ok := e.read(lifecycle.ScopeGlobal)
	if _, err := e.client("another creator").MarkDone(ctx, tool, lifecycle.ScopeGlobal, tb); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.SetIdleIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, lifecycle.ExpectOf(now, ok)); !errors.Is(err, lifecycle.ErrChanged) {
		t.Errorf("after a done: %v", err)
	}
	now, ok = e.read(lifecycle.ScopeGlobal)
	if it, err := e.c.SetIdleIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, lifecycle.ExpectOf(now, ok)); err != nil || it.State != lifecycle.StateIdle ||
		len(it.Marks) != 0 {
		t.Errorf("as read: %+v %v", it, err)
	}
	// A live lease is never removed, though its token is the one judged.
	l, err := e.c.Claim(ctx, tool, lifecycle.ScopeGlobal, 0)
	if err != nil {
		t.Fatal(err)
	}
	held, ok := e.read(lifecycle.ScopeGlobal)
	if _, err := e.c.SetIdleIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, lifecycle.ExpectOf(held, ok)); !errors.Is(err, lifecycle.ErrChanged) {
		t.Errorf("a live lease: %v", err)
	}
	if err := l.Step(ctx, "still mine"); err != nil {
		t.Errorf("the live lease was removed: %v", err)
	}
	// Stale: removed with it.
	e.advance(21 * time.Minute)
	if it, err := e.c.SetIdleIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, lifecycle.ExpectOf(held, ok)); err != nil || it.Lease != nil {
		t.Errorf("a stale lease: %+v %v", it, err)
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

// TestRetries: a throttle or a conflicting transaction is tried again by
// MarkDone and SetIdleIfUnchanged, and gives up after a few.
func TestRetries(t *testing.T) {
	ctx := context.Background()
	conflict := &types.TransactionCanceledException{Message: aws.String("conflict"),
		CancellationReasons: []types.CancellationReason{{Code: aws.String("None")}, {Code: aws.String("TransactionConflict")}}}
	throttle := &smithy.GenericAPIError{Code: "ThrottlingException", Message: "slow down"}
	for name, failure := range map[string]error{"conflict": conflict, "throttle": throttle} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			tok, _ := e.c.Mark(ctx, tool, region)
			n := 0
			e.fakes[region].Err = func(op string, _ any) error {
				if (op == "TransactWriteItems" || (op == "UpdateItem" && failure == throttle)) && n < 2 {
					n++
					return failure
				}
				return nil
			}
			if done, err := e.c.MarkDone(ctx, tool, region, tok); !done || err != nil {
				t.Errorf("MarkDone after %d failures: %v %v", n, done, err)
			}
			e2 := newEnv(t)
			n = 0
			e2.fakes[control].Err = func(op string, _ any) error {
				if op == "TransactWriteItems" && n < 2 {
					n++
					return failure
				}
				return nil
			}
			if _, err := e2.c.SetIdleIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, lifecycle.Expect{}); err != nil {
				t.Errorf("SetIdleIfUnchanged after %d failures: %v", n, err)
			}
			// Always failing: it gives up, and says why.
			e3 := newEnv(t)
			tok, _ = e3.c.Mark(ctx, tool, region)
			e3.fakes[region].Err = func(op string, _ any) error {
				if op == "TransactWriteItems" || (op == "UpdateItem" && failure == throttle) {
					return failure
				}
				return nil
			}
			if _, err := e3.c.MarkDone(ctx, tool, region, tok); err == nil {
				t.Error("MarkDone never gives up")
			}
			if _, err := e3.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.Expect{Exists: true}); err == nil || errors.Is(err, lifecycle.ErrChanged) {
				t.Errorf("SetIdleIfUnchanged never gives up: %v", err)
			}
		})
	}
	// The idle step alone failing for good.
	e := newEnv(t)
	tok, _ := e.c.Mark(ctx, tool, region)
	e.fakes[region].Err = func(op string, _ any) error {
		if op == "TransactWriteItems" {
			return conflict
		}
		return nil
	}
	if _, err := e.c.MarkDone(ctx, tool, region, tok); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Errorf("idle step conflicting: %v", err)
	}
	e.fakes[region].Err = nil
	tok, _ = e.client("x").Mark(ctx, tool, region)
	e.fakes[region].Err = func(op string, _ any) error {
		if op == "TransactWriteItems" {
			return errors.New("network down")
		}
		return nil
	}
	if _, err := e.c.MarkDone(ctx, tool, region, tok); err == nil {
		t.Error("idle step failing")
	}
}

// TestClaimIfUnchanged: a claim only at the epoch, state, lease and marks
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
	// Expected absent, but there: changed.
	var nc *lifecycle.NotClaimableError
	if _, err := e.c.ClaimIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, 0, lifecycle.Expect{}); !errors.As(err, &nc) || nc.Reason != "changed since it was judged" {
		t.Errorf("expected absent: %v", err)
	}
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
	held, ok := e.read(lifecycle.ScopeGlobal)
	if _, err := e.client("other one").ClaimIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, 0, lifecycle.ExpectOf(held, ok)); !errors.Is(err, lifecycle.ErrNotClaimable) {
		t.Errorf("a live lease: %v", err)
	}
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
	if _, err := e.c.ClaimIfUnchanged(ctx, tool, lifecycle.ScopeGlobal, 0, lifecycle.Expect{}); !errors.As(err, &nc) || nc.Reason != "no lifecycle item" {
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
}

// TestLockAndAdopt: a lock excludes claims and leaves the state as it was;
// an adopted lease is the caller's own live one, and can be given back.
func TestLockAndAdopt(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.c.Mark(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	it, ok := e.read(region)
	l, err := e.c.LockIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := e.read(region); got.State != lifecycle.StateActive || got.Lease == nil {
		t.Errorf("locked: %+v", got)
	}
	if _, err := e.client("a second run").LockIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok)); !errors.Is(err, lifecycle.ErrNotClaimable) {
		t.Errorf("a second lock: %v", err)
	}
	if err := l.Step(ctx, "delete a job"); err != nil {
		t.Error(err)
	}
	if err := l.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.read(region); got.State != lifecycle.StateActive || got.Lease != nil || len(got.Marks) != 1 {
		t.Errorf("unlocked: %+v", got)
	}
	if _, err := e.c.LockIfUnchanged(ctx, tool, region, lifecycle.Expect{}); !errors.Is(err, lifecycle.ErrNotClaimable) {
		t.Errorf("no item: %v", err)
	}
	if _, err := e.c.LockIfUnchanged(ctx, tool, "", lifecycle.Expect{Exists: true}); err == nil {
		t.Error("no scope")
	}
	e.fakes[region].Err = func(op string, _ any) error {
		if op == "UpdateItem" {
			return errors.New("network down")
		}
		return nil
	}
	it, ok = e.read(region)
	if _, err := e.c.LockIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok)); err == nil || errors.Is(err, lifecycle.ErrNotClaimable) {
		t.Errorf("a failing lock: %v", err)
	}
	e.fakes[region].Err = nil

	// Adopt: only a live lease in this client's name.
	e2 := newEnv(t)
	if _, _, err := e2.c.Observe(ctx, tool, region, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e2.c.AdoptLease(ctx, tool, region); !errors.Is(err, lifecycle.ErrLeaseLost) {
		t.Errorf("no lease: %v", err)
	}
	if _, err := e2.c.Claim(ctx, tool, region, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e2.client("someone else").AdoptLease(ctx, tool, region); !errors.Is(err, lifecycle.ErrLeaseLost) {
		t.Errorf("another's lease: %v", err)
	}
	mine, err := e2.c.AdoptLease(ctx, tool, region)
	if err != nil {
		t.Fatal(err)
	}
	if err := mine.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := e2.read(region); got.Lease != nil {
		t.Errorf("adopted and released: %+v", got)
	}
	if _, err := e2.c.AdoptLease(ctx, tool, ""); err == nil {
		t.Error("no scope")
	}
	e2.fakes[region].Err = func(string, any) error { return errors.New("network down") }
	if _, err := e2.c.AdoptLease(ctx, tool, region); err == nil || errors.Is(err, lifecycle.ErrLeaseLost) {
		t.Errorf("a failing read: %v", err)
	}
}

// TestConditionalEdges: the remaining error paths.
func TestConditionalEdges(t *testing.T) {
	ctx := context.Background()
	if e := lifecycle.ExpectOf(lifecycle.Item{Epoch: 3}, false); e.Exists || e.Epoch != 0 {
		t.Errorf("ExpectOf absent = %+v", e)
	}
	e := newEnv(t)
	if _, err := e.c.Mark(ctx, tool, ""); err == nil {
		t.Error("Mark with no scope")
	}
	if _, err := e.c.MarkDone(ctx, tool, "", "x"); err == nil {
		t.Error("MarkDone with no scope")
	}
	if _, err := e.c.SetIdleIfUnchanged(ctx, tool, "", lifecycle.Expect{}); err == nil {
		t.Error("SetIdleIfUnchanged with no scope")
	}
	if _, err := e.c.ClaimIfUnchanged(ctx, tool, "", 0, lifecycle.Expect{}); err == nil {
		t.Error("ClaimIfUnchanged with no scope")
	}
	// The table cannot be created for an absent item: not ErrChanged.
	e.fakes[region].Err = func(op string, _ any) error {
		if op == "CreateTable" {
			return errors.New("AccessDenied")
		}
		return nil
	}
	if _, err := e.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.Expect{}); err == nil || errors.Is(err, lifecycle.ErrChanged) {
		t.Errorf("no table, and none can be made: %v", err)
	}
	// The table keeps vanishing: a not-found error, not ErrChanged.
	e5 := newEnv(t)
	e5.fakes[region].Err = func(op string, _ any) error {
		if op == "TransactWriteItems" {
			return &types.ResourceNotFoundException{Message: aws.String("gone")}
		}
		return nil
	}
	if _, err := e5.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.Expect{}); err == nil || errors.Is(err, lifecycle.ErrChanged) {
		t.Errorf("a table that keeps vanishing: %v", err)
	}
	// An item expected but gone with its table: changed.
	if _, err := newEnv(t).c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.Expect{Exists: true, State: lifecycle.StateIdle}); !errors.Is(err, lifecycle.ErrChanged) {
		t.Errorf("no table for an existing item: %v", err)
	}
	// A creator marks between the claim's read and its write.
	e3 := newEnv(t)
	if _, err := e3.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.Expect{}); err != nil {
		t.Fatal(err)
	}
	it, ok := e3.read(region)
	raced := false
	e3.fakes[region].Hook = func(ctx context.Context, op string) {
		if op == "UpdateItem" && !raced {
			raced = true
			if _, err := e3.client("a creator").Mark(ctx, tool, region); err != nil {
				t.Error(err)
			}
		}
	}
	if _, err := e3.c.ClaimIfUnchanged(ctx, tool, region, 0, lifecycle.ExpectOf(it, ok)); !errors.Is(err, lifecycle.ErrNotClaimable) {
		t.Errorf("a mark while claiming: %v", err)
	}
	e4 := newEnv(t)
	if _, err := e4.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.Expect{}); err != nil {
		t.Fatal(err)
	}
	it, ok = e4.read(region)
	e4.fakes[region].Err = func(op string, _ any) error {
		if op == "UpdateItem" {
			return &types.ResourceNotFoundException{Message: aws.String("gone")}
		}
		return nil
	}
	if _, err := e4.c.ClaimIfUnchanged(ctx, tool, region, 0, lifecycle.ExpectOf(it, ok)); !errors.Is(err, lifecycle.ErrNotClaimable) {
		t.Errorf("a table gone while claiming: %v", err)
	}
	if _, err := e4.c.LockIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok)); !errors.Is(err, lifecycle.ErrNotClaimable) {
		t.Errorf("a table gone while locking: %v", err)
	}
	e6 := newEnv(t, func(o *lifecycle.Options) { o.Rand = failingReader{} })
	if _, err := e6.c.Mark(ctx, tool, region); err == nil {
		t.Error("Mark with no randomness")
	}
	if _, err := e6.c.LockIfUnchanged(ctx, tool, region, lifecycle.Expect{Exists: true}); err == nil {
		t.Error("Lock with no randomness")
	}
	// A done while the table closes.
	e7 := newEnv(t)
	tok, _ := e7.c.Mark(ctx, tool, region)
	putSentinel(t, e7, t0)
	if _, err := e7.c.MarkDone(ctx, tool, region, tok); !errors.Is(err, lifecycle.ErrTableClosing) {
		t.Errorf("closing: %v", err)
	}
	_ = tokens
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }

// TestRemoveHoldIdleFails: the hold goes; an idle step that fails is an
// error, one that meets a closing table is not; a mark stored without a time
// decodes with none.
func TestRemoveHoldIdleFails(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if err := e.c.AddHold(ctx, tool, region, "spawn"); err != nil {
		t.Fatal(err)
	}
	e.fakes[region].Err = func(op string, _ any) error {
		if op == "TransactWriteItems" {
			return errors.New("network down")
		}
		return nil
	}
	if err := e.c.RemoveHold(ctx, tool, region, "spawn"); err == nil {
		t.Error("a failing idle step")
	}
	e.fakes[region].Err = func(op string, _ any) error {
		if op == "UpdateItem" {
			return errors.New("network down")
		}
		return nil
	}
	if err := e.c.RemoveHold(ctx, tool, region, "spawn"); err == nil {
		t.Error("a failing delete")
	}
	e2 := newEnv(t)
	if err := e2.c.AddHold(ctx, tool, region, "spawn"); err != nil {
		t.Fatal(err)
	}
	putSentinel(t, e2, t0)
	if err := e2.c.RemoveHold(ctx, tool, region, "spawn"); err != nil {
		t.Errorf("closing: %v", err)
	}
	e3 := newEnv(t)
	if err := e3.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	if err := e3.fake().PutRaw(lifecycle.TableName, map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: "tool#lagotto#us-west-2"}, "state": &types.AttributeValueMemberS{Value: "ACTIVE"},
		"marks": &types.AttributeValueMemberSS{Value: []string{"legacy"}},
	}); err != nil {
		t.Fatal(err)
	}
	if it, _ := e3.read(region); len(it.Marks) != 1 || it.Marks[0].Token != "legacy" || !it.Marks[0].At.IsZero() {
		t.Errorf("a mark with no time: %+v", it.Marks)
	}
}
