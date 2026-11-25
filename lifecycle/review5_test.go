package lifecycle_test

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/spore-host/libs/lifecycle"
)

// The fifth review of #89.

// TestReview5UntimedMarksAge (L1): a mark with no readable time is aged from
// the item's ActiveAt or LastActivityAt; an idle Observe removes it once it
// is past the bound, so the item never stays in use for ever.
func TestReview5UntimedMarksAge(t *testing.T) {
	ctx := context.Background()
	for mark, at := range map[string]types.AttributeValue{"no time": &types.AttributeValueMemberN{Value: "1"}, "tok": &types.AttributeValueMemberS{Value: "not-a-time"}} {
		e := newEnv(t)
		if err := e.c.MarkActive(ctx, tool, region); err != nil {
			t.Fatal(err)
		}
		if err := e.fake().PutRaw(lifecycle.TableName, map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: lifecycle.ItemKey(tool, region)}, "state": &types.AttributeValueMemberS{Value: "ACTIVE"},
			"epoch": &types.AttributeValueMemberN{Value: "1"}, "stampState": &types.AttributeValueMemberS{Value: "ACTIVE"},
			"stampEpoch": &types.AttributeValueMemberN{Value: "1"},
			"activeAt":   &types.AttributeValueMemberS{Value: "2026-11-16T12:00:00.000Z"},
			"marks":      &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{mark: at}},
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

// TestReview5RefreshMark (L2, and the sixth review's one-write form): a
// creator running past the bound refreshes its mark and stays in use; a
// refresh is a change to a judged item; a mark that is gone, or an item
// under another's live lease, is not refreshed. One write, no read, from
// any Client.
func TestReview5RefreshMark(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tok, err := e.c.Mark(ctx, tool, region)
	if err != nil {
		t.Fatal(err)
	}
	e.advance(bound / 3)
	judged, ok := e.read(region)
	reads := e.fake().Calls("GetItem")
	if done, err := e.client("another client").RefreshMark(ctx, tool, region, tok); !done || err != nil {
		t.Fatalf("refresh: %v %v", done, err)
	}
	if n := e.fake().Calls("GetItem") - reads; n != 0 {
		t.Errorf("%d reads", n)
	}
	it, _ := e.read(region)
	if len(it.Marks) != 1 || it.Marks[0].Token != tok || !it.Marks[0].At.Equal(t0.Add(bound/3)) || !it.LastActivityAt.Equal(t0.Add(bound/3)) {
		t.Fatalf("after the refresh: %+v", it)
	}
	e.advance(bound - time.Minute)
	if it, _ := e.read(region); !lifecycle.InUse(it, e.clock(), bound) {
		t.Errorf("a refreshed creator past its first bound: %+v", it)
	}
	if _, err := e.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(judged, ok)); !errors.Is(err, lifecycle.ErrChanged) {
		t.Errorf("a refresh is a change: %v", err)
	}
	if done, err := e.c.MarkDone(ctx, tool, region, tok); !done || err != nil {
		t.Fatal(done, err)
	}
	// Gone: not refreshed, nor ended again.
	if done, err := e.c.RefreshMark(ctx, tool, region, tok); done || err != nil {
		t.Errorf("a gone mark: %v %v", done, err)
	}
	if done, err := e.c.MarkDone(ctx, tool, region, tok); done || err != nil {
		t.Errorf("a second done: %v %v", done, err)
	}
	if done, err := newEnv(t).c.RefreshMark(ctx, tool, region, tok); done || err != nil {
		t.Errorf("no table: %v %v", done, err)
	}
	if it, ok := newEnv(t).read(region); ok {
		t.Errorf("a refresh made an item: %+v", it)
	}
	// Under a backstop's short lock: refreshed (the seventh review's L2);
	// under a teardown's live claim: not.
	tok2, _ := e.c.Mark(ctx, tool, region)
	it, ok = e.read(region)
	l, err := e.client("a backstop").LockIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok))
	if err != nil {
		t.Fatal(err)
	}
	e.advance(time.Second)
	if done, err := e.c.RefreshMark(ctx, tool, region, tok2); !done || err != nil {
		t.Errorf("under a lock: %v %v", done, err)
	}
	if err := l.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
	ec := newEnv(t)
	if err := ec.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	if err := ec.fake().PutRaw(lifecycle.TableName, map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: lifecycle.ItemKey(tool, region)}, "state": &types.AttributeValueMemberS{Value: "TEARDOWN"},
		"marks": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{"tok4": &types.AttributeValueMemberS{Value: "2026-11-16T12:00:00.000Z"}}},
		"lease": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
			"token": &types.AttributeValueMemberS{Value: "a claim"}, "heartbeat": &types.AttributeValueMemberS{Value: "2026-11-16T12:00:00.000Z"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if done, err := ec.c.RefreshMark(ctx, tool, region, "tok4"); done || err != nil {
		t.Errorf("under a claim: %v %v", done, err)
	}
	if it, _ := ec.read(region); len(it.Marks) != 1 || !it.Marks[0].At.Equal(t0) {
		t.Errorf("refreshed under a claim: %+v", it.Marks)
	}
	// A write that fails is an error.
	e.fake().Err = func(op string, _ any) error {
		if op == "UpdateItem" {
			return errors.New("network down")
		}
		return nil
	}
	if _, err := e.c.RefreshMark(ctx, tool, region, tok2); err == nil {
		t.Error("a failed write")
	}
	e.fake().Err = nil
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
