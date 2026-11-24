package lifecycle_test

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/spore-host/libs/lifecycle"
)

// The fifth review of #89.

// TestReview5UntimedMarksAge (L1): a mark with no readable time is aged from
// the item's ActiveAt or LastActivityAt; an idle Observe removes it once it
// is past the bound, so the item never stays in use for ever.
func TestReview5UntimedMarksAge(t *testing.T) {
	ctx := context.Background()
	for _, mark := range []string{"legacy", "tok|not-a-time"} {
		e := newEnv(t)
		if err := e.c.MarkActive(ctx, tool, region); err != nil {
			t.Fatal(err)
		}
		if err := e.fake().PutRaw(lifecycle.TableName, map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: lifecycle.ItemKey(tool, region)}, "state": &types.AttributeValueMemberS{Value: "ACTIVE"},
			"epoch": &types.AttributeValueMemberN{Value: "1"}, "stampState": &types.AttributeValueMemberS{Value: "ACTIVE"},
			"stampEpoch": &types.AttributeValueMemberN{Value: "1"},
			"activeAt":   &types.AttributeValueMemberS{Value: "2026-11-16T12:00:00.000Z"},
			"marks":      &types.AttributeValueMemberSS{Value: []string{mark}},
		}); err != nil {
			t.Fatal(err)
		}
		it, _ := e.read(region)
		if len(it.Marks) != 1 || !it.Marks[0].At.IsZero() || !lifecycle.InUse(it, e.clock(), bound) {
			t.Fatalf("%s: a fresh untimed mark: %+v", mark, it)
		}
		if _, _, err := e.c.Observe(ctx, tool, region, false); err != nil {
			t.Fatal(err)
		}
		if it, _ := e.read(region); len(it.Marks) != 1 {
			t.Errorf("%s: removed within the bound: %+v", mark, it)
		}
		e.advance(bound)
		it, _ = e.read(region)
		if lifecycle.InUse(it, e.clock(), bound) || !lifecycle.IdleSince(it, e.clock(), bound).Equal(t0.Add(bound)) {
			t.Errorf("%s: past the bound: %+v", mark, it)
		}
		if _, _, err := e.c.Observe(ctx, tool, region, false); err != nil {
			t.Fatal(err)
		}
		if it, _ := e.read(region); len(it.Marks) != 0 || it.State != lifecycle.StateIdle {
			t.Errorf("%s: an idle observe past the bound: %+v", mark, it)
		}
	}
}

// TestReview5RefreshMark (L2): a creator running past the bound refreshes
// its mark and stays in use; a refresh is a change to a judged item; a mark
// that is gone, or an item under a teardown's live lease, is not refreshed;
// a refresh whose second write fails leaves both times, which MarkDone
// removes together.
func TestReview5RefreshMark(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tok, err := e.c.Mark(ctx, tool, region)
	if err != nil {
		t.Fatal(err)
	}
	e.advance(bound / 3)
	judged, ok := e.read(region)
	if done, err := e.c.RefreshMark(ctx, tool, region, tok); !done || err != nil {
		t.Fatalf("refresh: %v %v", done, err)
	}
	if done, err := e.c.RefreshMark(ctx, tool, region, tok); !done || err != nil {
		t.Fatalf("a refresh at the same time: %v %v", done, err)
	}
	it, _ := e.read(region)
	if len(it.Marks) != 1 || it.Marks[0].Token != tok || !it.Marks[0].At.Equal(t0.Add(bound/3)) {
		t.Fatalf("after the refresh: %+v", it.Marks)
	}
	e.advance(bound - time.Minute)
	if it, _ := e.read(region); !lifecycle.InUse(it, e.clock(), bound) {
		t.Errorf("a refreshed creator past its first bound: %+v", it)
	}
	if _, err := e.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(judged, ok)); !errors.Is(err, lifecycle.ErrChanged) {
		t.Errorf("a refresh is a change: %v", err)
	}
	// The second write fails: both times, ended together.
	e.fake().Err = func(op string, in any) error {
		if u, ok := in.(*dynamodb.UpdateItemInput); ok && op == "UpdateItem" && *u.UpdateExpression == "DELETE #marks :old" {
			return errors.New("network down")
		}
		return nil
	}
	if done, err := e.c.RefreshMark(ctx, tool, region, tok); !done || err == nil {
		t.Errorf("a failed second write: %v %v", done, err)
	}
	e.fake().Err = nil
	if it, _ := e.read(region); len(it.Marks) != 2 {
		t.Errorf("both times: %+v", it.Marks)
	}
	if done, err := e.c.MarkDone(ctx, tool, region, tok); !done || err != nil {
		t.Fatal(done, err)
	}
	if it, _ := e.read(region); len(it.Marks) != 0 {
		t.Errorf("after the done: %+v", it.Marks)
	}
	// Gone: not refreshed.
	if done, err := e.c.RefreshMark(ctx, tool, region, tok); done || err != nil {
		t.Errorf("a gone mark: %v %v", done, err)
	}
	if done, err := newEnv(t).c.RefreshMark(ctx, tool, region, tok); done || err != nil {
		t.Errorf("no table: %v %v", done, err)
	}
	// Under another's live lease (a lock): not refreshed.
	tok2, _ := e.c.Mark(ctx, tool, region)
	it, ok = e.read(region)
	l, err := e.client("a backstop").LockIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok))
	if err != nil {
		t.Fatal(err)
	}
	if done, err := e.c.RefreshMark(ctx, tool, region, tok2); done || err != nil {
		t.Errorf("under a live lease: %v %v", done, err)
	}
	if err := l.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
	// The Client that wrote the mark refreshes and ends it with no read (a
	// CLI user's grant has no GetItem); any other reads it, and a read that
	// fails is an error.
	reads := e.fake().Calls("GetItem")
	if done, err := e.c.RefreshMark(ctx, tool, region, tok2); !done || err != nil {
		t.Errorf("unlocked: %v %v", done, err)
	}
	if done, err := e.c.MarkDone(ctx, tool, region, tok2); !done || err != nil {
		t.Errorf("done: %v %v", done, err)
	}
	if n := e.fake().Calls("GetItem") - reads; n != 0 {
		t.Errorf("%d reads by the mark's own Client", n)
	}
	tok3, _ := e.c.Mark(ctx, tool, region)
	other := e.client("another client")
	e.advance(time.Minute)
	if done, err := other.RefreshMark(ctx, tool, region, tok3); !done || err != nil {
		t.Errorf("another client's refresh: %v %v", done, err)
	}
	e.fake().Err = func(op string, _ any) error {
		if op == "GetItem" {
			return errors.New("network down")
		}
		return nil
	}
	if _, err := e.client("a third").RefreshMark(ctx, tool, region, tok3); err == nil {
		t.Error("a failed read")
	}
	if _, err := e.client("a fourth").MarkDone(ctx, tool, region, tok3); err == nil {
		t.Error("a failed read (done)")
	}
	e.fake().Err = nil
	if done, err := other.MarkDone(ctx, tool, region, tok3); !done || err != nil {
		t.Errorf("another client's done: %v %v", done, err)
	}
	if it, _ := e.read(region); len(it.Marks) != 0 {
		t.Errorf("left: %+v", it.Marks)
	}
}

// TestReview5LastActivityOnlyAdvances (L3): a writer whose clock is behind
// never moves lastActivityAt backwards: each write that stamps it, meeting
// a later stamp, stamps that one instead.
func TestReview5LastActivityOnlyAdvances(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	slow := e.client("slow clock", func(o *lifecycle.Options) { o.Now = func() time.Time { return e.clock().Add(-time.Hour) } })
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	stamp := func() time.Time { it, _ := e.read(region); return it.LastActivityAt }
	want := stamp()
	check := func(what string) {
		t.Helper()
		if got := stamp(); got.Before(want) {
			t.Errorf("%s moved lastActivityAt back to %v from %v", what, got, want)
		} else {
			want = got
		}
	}
	tok, err := slow.Mark(ctx, tool, region)
	if err != nil {
		t.Fatal(err)
	}
	check("Mark")
	if done, err := slow.MarkDone(ctx, tool, region, tok); !done || err != nil {
		t.Fatal(done, err)
	}
	check("MarkDone")
	if err := slow.AddHold(ctx, tool, region, "spawn"); err != nil {
		t.Fatal(err)
	}
	check("AddHold")
	if err := slow.RemoveHold(ctx, tool, region, "spawn"); err != nil {
		t.Fatal(err)
	}
	check("RemoveHold")
	if _, _, err := slow.Observe(ctx, tool, region, true); err != nil {
		t.Fatal(err)
	}
	check("an active Observe")
	if _, _, err := slow.Observe(ctx, tool, region, false); err != nil {
		t.Fatal(err)
	}
	l, err := slow.Claim(ctx, tool, region, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(ctx); err != nil {
		t.Fatal(err)
	}
	check("Release")
	// A later stamp written between a writer's read and its write: the
	// write stamps that one.
	e2 := newEnv(t)
	tok, _ = e2.c.Mark(ctx, tool, region)
	ahead := e2.client("ahead", func(o *lifecycle.Options) { o.Now = func() time.Time { return e2.clock().Add(time.Hour) } })
	raced := false
	e2.fake().Hook = func(ctx context.Context, op string) {
		if op == "UpdateItem" && !raced {
			raced = true
			if _, _, err := ahead.Observe(ctx, tool, region, true); err != nil {
				t.Error(err)
			}
		}
	}
	if done, err := e2.c.MarkDone(ctx, tool, region, tok); !done || err != nil {
		t.Errorf("a done that met a later stamp: %v %v", done, err)
	}
	e2.fake().Hook = nil
	if it, _ := e2.read(region); !it.LastActivityAt.Equal(t0.Add(time.Hour)) || len(it.Marks) != 0 {
		t.Errorf("after: %+v", it)
	}
	// An activation and an active Observe that meet a later stamp.
	e3 := newEnv(t)
	ahead3 := e3.client("ahead", func(o *lifecycle.Options) { o.Now = func() time.Time { return e3.clock().Add(time.Hour) } })
	if err := ahead3.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	if err := e3.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatalf("an activation behind the stamp: %v", err)
	}
	// An active Observe whose read is overtaken by a later stamp reads
	// again and records it.
	e3.fake().Hook = func(ctx context.Context, op string) {
		if op == "TransactWriteItems" {
			e3.fake().Hook = nil
			e3.advance(time.Minute)
			if err := ahead3.MarkActive(ctx, tool, region); err != nil {
				t.Error(err)
			}
		}
	}
	if _, _, err := e3.c.Observe(ctx, tool, region, true); err != nil {
		t.Fatalf("an observe overtaken: %v", err)
	}
	if it, _ := e3.read(region); !it.LastActivityAt.Equal(t0.Add(time.Hour + time.Minute)) {
		t.Errorf("after the overtaken observe: %+v", it)
	}
}

// TestReview5UnlockRefusesAClaim (L4): Unlock gives back only a lock; a
// claim's lease, taken or adopted, it refuses (ErrNotALock), and an adopted
// lock it gives back.
func TestReview5UnlockRefusesAClaim(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if _, _, err := e.c.Observe(ctx, tool, region, false); err != nil {
		t.Fatal(err)
	}
	l, err := e.c.Claim(ctx, tool, region, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Unlock(ctx); !errors.Is(err, lifecycle.ErrNotALock) {
		t.Errorf("a claim: %v", err)
	}
	adopted, err := e.c.AdoptLease(ctx, tool, region)
	if err != nil {
		t.Fatal(err)
	}
	if err := adopted.Unlock(ctx); !errors.Is(err, lifecycle.ErrNotALock) {
		t.Errorf("an adopted claim: %v", err)
	}
	if err := adopted.Release(ctx); err != nil {
		t.Fatal(err)
	}
	it, ok := e.read(region)
	if _, err := e.c.LockIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok)); err != nil {
		t.Fatal(err)
	}
	if it, _ := e.read(region); it.Lease == nil || it.Lease.Kind != "lock" {
		t.Fatalf("the lock: %+v", it.Lease)
	}
	lock, err := e.c.AdoptLease(ctx, tool, region)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Unlock(ctx); err != nil {
		t.Errorf("an adopted lock: %v", err)
	}
	if it, _ := e.read(region); it.Lease != nil || it.State != lifecycle.StateActive {
		t.Errorf("after: %+v", it)
	}
}

// TestReview5OneStaleness (L5): InUse and LeaseStale (and so noLiveLease,
// which compares the same stored strings) agree at the boundary.
func TestReview5OneStaleness(t *testing.T) {
	e := newEnv(t)
	for _, age := range []time.Duration{20*time.Minute - time.Millisecond, 20 * time.Minute, 20*time.Minute + time.Millisecond, 20*time.Minute + 500*time.Microsecond} {
		it := lifecycle.Item{State: lifecycle.StateTeardown, StampState: lifecycle.StateTeardown,
			Lease: &lifecycle.LeaseInfo{Heartbeat: e.clock().Add(-age)}}
		if e.c.InUse(it, bound) == e.c.LeaseStale(it) {
			t.Errorf("age %s: InUse %v, LeaseStale %v", age, e.c.InUse(it, bound), e.c.LeaseStale(it))
		}
	}
}

// TestReview5AppliedWrapsBoth (L6): ErrApplied and the read-back error are
// both in the chain.
func TestReview5AppliedWrapsBoth(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	down := errors.New("network down")
	e.fake().Err = func(op string, _ any) error {
		if op == "GetItem" {
			return down
		}
		return nil
	}
	_, err := e.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.Expect{})
	if !errors.Is(err, lifecycle.ErrApplied) || !errors.Is(err, down) {
		t.Errorf("%v", err)
	}
}

// TestReview5RandSerialised (L8): one Client used concurrently with a Rand
// that is not safe for concurrent use (math/rand): every read of it is
// serialised (the race detector checks).
func TestReview5RandSerialised(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, func(o *lifecycle.Options) { o.Rand = rand.New(rand.NewSource(1)) })
	if _, _, err := e.c.Observe(ctx, tool, region, false); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_, _ = e.c.Claim(ctx, tool, region, 0)
			} else {
				_, _ = e.c.Mark(ctx, tool, lifecycle.ScopeGlobal)
			}
		}(i)
	}
	wg.Wait()
}
