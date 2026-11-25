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
	"github.com/aws/smithy-go"

	"github.com/spore-host/libs/lifecycle"
)

// The ninth review of #89.

// TestReview9SetIdleRefusesALiveMark (L1): a set-idle of an item as judged
// but with a creator's live mark, or a fresh pulse, is ErrInUse, and the mark
// stays.
func TestReview9SetIdleRefusesALiveMark(t *testing.T) {
	ctx := context.Background()
	for _, how := range []string{"mark", "pulse"} {
		e := newEnv(t)
		if how == "mark" {
			if _, err := e.c.Mark(ctx, tool, region); err != nil {
				t.Fatal(err)
			}
		} else if err := e.c.MarkActive(ctx, tool, region); err != nil {
			t.Fatal(err)
		}
		it, ok := e.read(region)
		if _, err := e.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok)); !errors.Is(err, lifecycle.ErrInUse) {
			t.Errorf("%s: %v", how, err)
		}
		if after, _ := e.read(region); after.State != lifecycle.StateActive || len(after.Marks) != len(it.Marks) {
			t.Errorf("%s: written: %+v", how, after)
		}
		e.advance(bound)
		it, ok = e.read(region)
		if _, err := e.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok)); err != nil {
			t.Errorf("%s past the bound: %v", how, err)
		}
	}
}

// TestReview9RefreshNotAppliedUnderAClaim (L2): a refresh retried after a
// throttle, whose item a claim now holds, is not done, even when the mark
// carries this call's time.
func TestReview9RefreshNotAppliedUnderAClaim(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tok, _ := e.c.Mark(ctx, tool, region)
	e.advance(time.Minute)
	f := e.fake()
	f.Err = func(o string, in any) error {
		if u, ok := in.(*dynamodb.UpdateItemInput); ok && o == "UpdateItem" {
			f.Err = nil
			_, _ = f.UpdateItem(ctx, u) // applied: the mark has this call's time
			// Then a claim lands on the item (as a stale-lease takeover).
			raw := f.Item(lifecycle.TableName, lifecycle.ItemKey(tool, region))
			raw["state"] = &types.AttributeValueMemberS{Value: "TEARDOWN"}
			raw["lease"] = &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
				"token": &types.AttributeValueMemberS{Value: "a claim"}, "heartbeat": &types.AttributeValueMemberS{Value: "2026-11-16T12:01:00.000Z"}}}
			if err := f.PutRaw(lifecycle.TableName, raw); err != nil {
				t.Error(err)
			}
			return &smithy.GenericAPIError{Code: "ThrottlingException", Message: "slow down"}
		}
		return nil
	}
	if done, err := e.c.RefreshMark(ctx, tool, region, tok); done || err != nil {
		t.Errorf("refreshed under a claim: %v %v", done, err)
	}
}

// TestReview9IdleSinceProperty (L3): for any item, IdleSince is zero only
// while it is in use or it has no time at all; never before its earliest
// recorded time; and never zero for one, not in use, with a stored
// idleSince.
func TestReview9IdleSinceProperty(t *testing.T) {
	r := rand.New(rand.NewSource(9))
	now := t0
	pick := func() time.Time {
		if r.Intn(3) == 0 {
			return time.Time{}
		}
		return now.Add(-time.Duration(r.Intn(48*60)) * time.Minute)
	}
	states := []lifecycle.State{"", lifecycle.StateActive, lifecycle.StateIdle, lifecycle.StateTeardown}
	for i := 0; i < 20000; i++ {
		it := lifecycle.Item{State: states[r.Intn(4)], ActiveAt: pick(), PulseAt: pick(), LastActivityAt: pick(), IdleSince: pick()}
		it.StampState = it.State
		if r.Intn(4) == 0 {
			it.StampState = ""
		}
		for n := r.Intn(3); n > 0; n-- {
			it.Marks = append(it.Marks, lifecycle.MarkInfo{Token: "m", At: pick()})
		}
		if r.Intn(2) == 0 {
			it.Lease = &lifecycle.LeaseInfo{Token: "l", Heartbeat: pick()}
		}
		got := lifecycle.IdleSince(it, now, bound)
		if lifecycle.InUse(it, now, bound) {
			if !got.IsZero() {
				t.Fatalf("in use, idle since %v: %+v", got, it)
			}
			continue
		}
		var earliest time.Time
		for _, x := range []time.Time{it.ActiveAt, it.PulseAt, it.LastActivityAt, it.IdleSince} {
			if !x.IsZero() && (earliest.IsZero() || x.Before(earliest)) {
				earliest = x
			}
		}
		if !got.IsZero() && !earliest.IsZero() && got.Before(earliest) && !onlyMarksOrLease(it, got) {
			t.Fatalf("idle since %v, before its earliest time %v: %+v", got, earliest, it)
		}
		if got.Year() < 2000 && !got.IsZero() {
			t.Fatalf("idle since %v (a zero time aged): %+v", got, it)
		}
		if !it.IdleSince.IsZero() && got.IsZero() {
			t.Fatalf("a stored idleSince, but idle since zero: %+v", it)
		}
	}
}

// onlyMarksOrLease reports whether got comes from a mark's or the lease's
// own time (each older than the item's other times is still a time it
// recorded).
func onlyMarksOrLease(it lifecycle.Item, got time.Time) bool {
	for _, m := range it.Marks {
		if !m.At.IsZero() && !got.Before(m.At) {
			return true
		}
	}
	return it.Lease != nil && !it.Lease.Heartbeat.IsZero() && !got.Before(it.Lease.Heartbeat)
}

// TestReview9ActiveObserveSkipsAFreshStamp (L4): an active Observe whose only
// change would be a fresh stamp writes nothing.
func TestReview9ActiveObserveSkipsAFreshStamp(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if _, _, err := e.c.Observe(ctx, tool, region, true); err != nil {
		t.Fatal(err)
	}
	writes := e.fake().Calls("TransactWriteItems")
	e.advance(30 * time.Second)
	if _, _, err := e.c.Observe(ctx, tool, region, true); err != nil {
		t.Fatal(err)
	}
	if n := e.fake().Calls("TransactWriteItems") - writes; n != 0 {
		t.Errorf("%d writes for a fresh stamp", n)
	}
	e.advance(time.Minute)
	if it, _, err := e.c.Observe(ctx, tool, region, true); err != nil || !it.LastActivityAt.Equal(e.clock()) {
		t.Errorf("a stale stamp: %+v %v", it, err)
	}
}

// TestReview9UntimedMarkAgesFromActiveAt (L6): a mark with no time is not
// kept alive by later writes' lastActivityAt.
func TestReview9UntimedMarkAgesFromActiveAt(t *testing.T) {
	it := lifecycle.Item{State: lifecycle.StateActive, StampState: lifecycle.StateActive, ActiveAt: t0.Add(-2 * bound), LastActivityAt: t0,
		Marks: []lifecycle.MarkInfo{{Token: "no time"}}}
	if lifecycle.InUse(it, t0, bound) {
		t.Error("kept alive by lastActivityAt")
	}
}

// TestReview9ClientIDConcurrent (L7): the Client id is made once, outside
// its lock, under concurrency (the race detector checks).
func TestReview9ClientIDConcurrent(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, func(o *lifecycle.Options) { o.Rand = rand.New(rand.NewSource(3)) })
	for _, r := range []string{region} {
		if _, _, err := e.c.Observe(ctx, tool, r, false); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = e.c.Claim(ctx, tool, region, 0) }()
	}
	wg.Wait()
	if it, _ := e.read(region); it.Lease == nil || it.Lease.Client == "" {
		t.Errorf("%+v", it.Lease)
	}
}
