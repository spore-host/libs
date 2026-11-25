package lifecycle_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/spore-host/libs/lifecycle"
)

// The sixth review of #89: marks are a map, lastActivityAt is the one
// change detector.

// TestReview6MarksAreAMap (S1): a mark is marks[token] = time, made on an
// absent item, on one with no marks map, and beside others; ended by one
// REMOVE; neither Mark nor MarkDone reads.
func TestReview6MarksAreAMap(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	a, err := e.c.Mark(ctx, tool, region) // absent item: the whole map
	if err != nil {
		t.Fatal(err)
	}
	raw := e.fake().Item(lifecycle.TableName, lifecycle.ItemKey(tool, region))
	m, ok := raw["marks"].(*types.AttributeValueMemberM)
	if !ok || len(m.Value) != 1 || m.Value[a] == nil {
		t.Fatalf("stored: %+v", raw["marks"])
	}
	b, err := e.client("the creator b").Mark(ctx, tool, region) // beside it
	if err != nil {
		t.Fatal(err)
	}
	e2 := newEnv(t)
	if err := e2.c.MarkActive(ctx, tool, region); err != nil { // an item with no map
		t.Fatal(err)
	}
	if _, err := e2.c.Mark(ctx, tool, region); err != nil {
		t.Fatalf("no map yet: %v", err)
	}
	if it, _ := e2.read(region); len(it.Marks) != 1 {
		t.Errorf("no map yet: %+v", it.Marks)
	}
	reads := e.fake().Calls("GetItem")
	if done, err := e.c.MarkDone(ctx, tool, region, a); !done || err != nil {
		t.Fatal(done, err)
	}
	if n := e.fake().Calls("GetItem") - reads; n != 0 {
		t.Errorf("MarkDone read %d times", n)
	}
	if it, _ := e.read(region); len(it.Marks) != 1 || it.Marks[0].Token != b {
		t.Errorf("after one done: %+v", it.Marks)
	}
	// A Mark that meets a live lease waits for it, as before, and says so.
	e3 := newEnv(t)
	e3.idleAndClaim(t)
	_, err = e3.client("late").Mark(ctx, tool, region)
	var we *lifecycle.WaitError
	if !errors.As(err, &we) || we.Blocker != "lease" {
		t.Errorf("a mark behind a live lease: %v", err)
	}
}

// TestReview6OneChangeDetector (S3): an idle Observe, a Claim, a
// ClaimIfUnchanged and a LockIfUnchanged are each conditional on
// lastActivityAt as read: a mark refreshed after the read (finding 5) keeps
// the item, and the mark.
func TestReview6OneChangeDetector(t *testing.T) {
	ctx := context.Background()
	race := func(e *env, op string, change func(ctx context.Context)) {
		e.fake().Hook = func(ctx context.Context, o string) {
			if o == op {
				e.fake().Hook = nil
				e.advance(time.Millisecond)
				change(ctx)
			}
		}
	}
	// The idle Observe: a dead mark, refreshed between its read and write.
	e := newEnv(t)
	tok, _ := e.c.Mark(ctx, tool, region)
	e.advance(bound)
	race(e, "TransactWriteItems", func(ctx context.Context) {
		if ok, err := e.client("creator").RefreshMark(ctx, tool, region, tok); !ok || err != nil {
			t.Error(ok, err)
		}
	})
	if _, _, err := e.c.Observe(ctx, tool, region, false); err != nil {
		t.Fatal(err)
	}
	if it, _ := e.read(region); len(it.Marks) != 1 || it.State != lifecycle.StateActive || !lifecycle.InUse(it, e.clock(), bound) {
		t.Errorf("an idle observe deleted a refreshed mark: %+v", it)
	}
	// Claim: an IDLE item whose (dead) mark is ended between read and write,
	// which moves no epoch.
	e = newEnv(t)
	if _, _, err := e.c.Observe(ctx, tool, region, false); err != nil {
		t.Fatal(err)
	}
	if err := e.fake().PutRaw(lifecycle.TableName, map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: lifecycle.ItemKey(tool, region)}, "state": &types.AttributeValueMemberS{Value: "IDLE"},
		"idleSince": &types.AttributeValueMemberS{Value: "2026-11-15T00:00:00.000Z"}, "stampState": &types.AttributeValueMemberS{Value: "IDLE"},
		"marks": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{"dead": &types.AttributeValueMemberS{Value: "2026-11-15T00:00:00.000Z"}}},
	}); err != nil {
		t.Fatal(err)
	}
	race(e, "UpdateItem", func(ctx context.Context) {
		if done, err := e.client("x").MarkDone(ctx, tool, region, "dead"); !done || err != nil {
			t.Error(done, err)
		}
	})
	if _, err := e.c.Claim(ctx, tool, region, 0); !errors.Is(err, lifecycle.ErrNotClaimable) {
		t.Errorf("a Claim after a change: %v", err)
	}
	// ClaimIfUnchanged and LockIfUnchanged with a judgement made before a
	// stamp (an active Observe, which bumps no epoch).
	e = newEnv(t)
	if _, err := e.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.Expect{}); err != nil {
		t.Fatal(err)
	}
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	judged, ok := e.read(region)
	e.advance(time.Minute)
	if _, _, err := e.client("poller").Observe(ctx, tool, region, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.LockIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(judged, ok)); !errors.Is(err, lifecycle.ErrNotClaimable) {
		t.Errorf("a Lock after a stamp: %v", err)
	}
	if _, err := e.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(judged, ok)); !errors.Is(err, lifecycle.ErrChanged) {
		t.Errorf("a set-idle after a stamp: %v", err)
	}
}

// TestReview6StampIsTheWritersClock (S2): lastActivityAt is set with the
// writer's own clock, plainly: a writer whose clock is behind sets it
// earlier (clocks are assumed to agree within minutes, far inside the
// bounds), and the lease staleness it writes is from its own clock.
func TestReview6StampIsTheWritersClock(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	slow := e.client("slow", func(o *lifecycle.Options) { o.Now = func() time.Time { return e.clock().Add(-time.Minute) } })
	e.advance(3 * time.Minute) // past the fresh-stamp skip, for the slow clock too
	if _, _, err := slow.Observe(ctx, tool, region, true); err != nil {
		t.Fatal(err)
	}
	if it, _ := e.read(region); !it.LastActivityAt.Equal(t0.Add(2 * time.Minute)) {
		t.Errorf("stamp: %v", it.LastActivityAt)
	}
}

// TestReview6LeaseKinds (S4): a lock is not taken on a TEARDOWN item, and a
// lock is not given back with Release or Suspend.
func TestReview6LeaseKinds(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	l := e.idleAndClaim(t)
	it, ok := e.read(region)
	e.advance(21 * time.Minute) // its lease stale
	if _, err := e.client("a backstop").LockIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok)); !errors.Is(err, lifecycle.ErrNotClaimable) {
		t.Errorf("a lock on TEARDOWN: %v", err)
	}
	_ = l
	e = newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	it, ok = e.read(region)
	lock, err := e.c.LockIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok))
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(ctx); !errors.Is(err, lifecycle.ErrNotALock) {
		t.Errorf("Release of a lock: %v", err)
	}
	if err := lock.Suspend(ctx); !errors.Is(err, lifecycle.ErrNotALock) {
		t.Errorf("Suspend of a lock: %v", err)
	}
	if err := lock.Unlock(ctx); err != nil {
		t.Errorf("Unlock: %v", err)
	}
}
