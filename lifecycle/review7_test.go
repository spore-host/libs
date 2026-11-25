package lifecycle_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"

	"github.com/spore-host/libs/lifecycle"
)

// The seventh review of #89.

// TestReview7FinishRefusesALock (L1): a lock never deletes the item.
func TestReview7FinishRefusesALock(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	it, ok := e.read(region)
	lock, err := e.c.LockIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok))
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Finish(ctx); !errors.Is(err, lifecycle.ErrNotALock) {
		t.Errorf("Finish of a lock: %v", err)
	}
	if _, ok := e.read(region); !ok {
		t.Error("the item was deleted")
	}
}

// TestReview7RemoveAbsentHoldWritesNothing (L3): removing a hold that is not
// there is a no-op: no stamp, so no idle clock restarted and no judgement
// disturbed.
func TestReview7RemoveAbsentHoldWritesNothing(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	before, _ := e.read(region)
	e.advance(time.Minute)
	if err := e.c.RemoveHold(ctx, tool, region, "spawn"); err != nil {
		t.Fatal(err)
	}
	if it, _ := e.read(region); !it.LastActivityAt.Equal(before.LastActivityAt) {
		t.Errorf("a no-op stamped: %v -> %v", before.LastActivityAt, it.LastActivityAt)
	}
	if err := e.c.AddHold(ctx, tool, region, "spawn"); err != nil {
		t.Fatal(err)
	}
	e.advance(time.Minute)
	if err := e.c.RemoveHold(ctx, tool, region, "spawn"); err != nil {
		t.Fatal(err)
	}
	if it, _ := e.read(region); len(it.Holds) != 0 || !it.LastActivityAt.Equal(e.clock()) {
		t.Errorf("a real removal: %+v", it)
	}
}

// TestReview7MarkShapeFollowsTheItem (L4): a Mark whose item keeps
// changing shape (the marks map removed and made again between its
// attempts) follows it, however often, and is never mistaken for a lease
// wait; a marks attribute that is not a map is an error.
func TestReview7MarkShapeFollowsTheItem(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	other := e.client("other")
	tok, _ := other.Mark(ctx, tool, region)
	flips := 0
	var hook func(ctx context.Context, op string)
	hook = func(ctx context.Context, op string) {
		if op != "TransactWriteItems" || flips >= 6 {
			return
		}
		flips++
		e.fake().Hook = nil
		if flips%2 == 1 {
			_, _ = other.MarkDone(ctx, tool, region, tok)
			it, ok := e.read(region)
			if _, err := other.SetIdleIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok)); err != nil { // no map
				t.Error(err)
			}
		} else {
			tok, _ = other.Mark(ctx, tool, region) // the map again
		}
		e.fake().Hook = hook
	}
	e.fake().Hook = hook
	if _, err := e.c.Mark(ctx, tool, region); err != nil {
		t.Fatalf("after %d flips: %v", flips, err)
	}
	e.fake().Hook = nil
	if flips < 4 {
		t.Errorf("only %d flips", flips)
	}
	e2 := newEnv(t)
	if err := e2.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	if err := e2.fake().PutRaw(lifecycle.TableName, map[string]types.AttributeValue{
		"pk":    &types.AttributeValueMemberS{Value: lifecycle.ItemKey(tool, region)},
		"marks": &types.AttributeValueMemberSS{Value: []string{"x"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e2.c.Mark(ctx, tool, region); err == nil {
		t.Error("a marks attribute that is not a map")
	}
}

// TestReview7SetIdleClearsADeadSentinel (L5): a closing sentinel a closer
// that died left is cleared, and the idle write goes ahead.
func TestReview7SetIdleClearsADeadSentinel(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	putSentinel(t, e, t0.Add(-time.Hour))
	it, ok := e.read(region)
	if got, err := e.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok)); err != nil || got.State != lifecycle.StateIdle {
		t.Errorf("%+v %v", got, err)
	}
	// A live closer's is left: closing.
	e2 := newEnv(t)
	if err := e2.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	putSentinel(t, e2, t0)
	it, ok = e2.read(region)
	if _, err := e2.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok)); !errors.Is(err, lifecycle.ErrTableClosing) {
		t.Errorf("a live closer: %v", err)
	}
}

// TestReview7RetriedWriteThatApplied (L6): a done or a refresh whose first
// try applied, though its answer was a throttle, reports done on the retry.
func TestReview7RetriedWriteThatApplied(t *testing.T) {
	ctx := context.Background()
	throttle := &smithy.GenericAPIError{Code: "ThrottlingException", Message: "slow down"}
	for _, op := range []string{"done", "refresh"} {
		e := newEnv(t)
		tok, _ := e.c.Mark(ctx, tool, region)
		e.advance(time.Minute)
		f := e.fake()
		f.Err = func(o string, in any) error {
			if u, ok := in.(*dynamodb.UpdateItemInput); ok && o == "UpdateItem" {
				f.Err = nil
				_, _ = f.UpdateItem(ctx, u)
				return throttle
			}
			return nil
		}
		var done bool
		var err error
		if op == "done" {
			done, err = e.c.MarkDone(ctx, tool, region, tok)
		} else {
			done, err = e.c.RefreshMark(ctx, tool, region, tok)
		}
		if !done || err != nil {
			t.Errorf("%s: %v %v", op, done, err)
		}
	}
	// A refresh retried after a throttle whose mark is gone is not done.
	e := newEnv(t)
	tok, _ := e.c.Mark(ctx, tool, region)
	f := e.fake()
	f.Err = func(o string, in any) error {
		if o == "UpdateItem" {
			f.Err = nil
			if _, err := e.client("x").MarkDone(ctx, tool, region, tok); err != nil {
				t.Error(err)
			}
			return throttle
		}
		return nil
	}
	if done, err := e.c.RefreshMark(ctx, tool, region, tok); done || err != nil {
		t.Errorf("a gone mark: %v %v", done, err)
	}
}

// TestReview7EmptyToken (L8): no token, no request.
func TestReview7EmptyToken(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	for _, f := range []func() (bool, error){
		func() (bool, error) { return e.c.MarkDone(ctx, tool, region, "") },
		func() (bool, error) { return e.c.RefreshMark(ctx, tool, region, "") },
	} {
		if done, err := f(); done || err != nil {
			t.Errorf("%v %v", done, err)
		}
	}
	if n := e.fake().Calls("UpdateItem"); n != 0 {
		t.Errorf("%d requests", n)
	}
}
