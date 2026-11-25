package lifecycle_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/spore-host/libs/lifecycle"
)

// The eighth review of #89.

// capOps fails the test once the env's region table has served more than n
// operations: a retry loop that does not end shows as a failure, not a hang.
func (e *env) capOps(n int) {
	f := e.fake()
	ops, prev := 0, f.Err
	f.Err = func(op string, in any) error {
		if ops++; ops > n {
			e.t.Errorf("more than %d operations: a loop that does not end", n)
			return errors.New("capOps: too many operations")
		}
		if prev != nil {
			return prev(op, in)
		}
		return nil
	}
}

// TestReview8StaleHeartbeatOneRule (L1): a lease whose heartbeat is
// missing, or is not a time, is judged by one rule, the stored strings'
// order, by the Go side and by the condition alike: a Mark behind it
// neither spins nor waits forever.
func TestReview8StaleHeartbeatOneRule(t *testing.T) {
	ctx := context.Background()
	for name, hb := range map[string]types.AttributeValue{
		"missing":             nil,
		"not a time, early":   &types.AttributeValueMemberS{Value: "0-garbage"},
		"not a time, late":    &types.AttributeValueMemberS{Value: "zzz"},
		"not a string at all": &types.AttributeValueMemberN{Value: "1"},
	} {
		e := newEnv(t)
		if err := e.c.MarkActive(ctx, tool, region); err != nil {
			t.Fatal(err)
		}
		lease := map[string]types.AttributeValue{"token": &types.AttributeValueMemberS{Value: "x"}}
		if hb != nil {
			lease["heartbeat"] = hb
		}
		if err := e.fake().PutRaw(lifecycle.TableName, map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: lifecycle.ItemKey(tool, region)}, "state": &types.AttributeValueMemberS{Value: "TEARDOWN"},
			"lease": &types.AttributeValueMemberM{Value: lease},
		}); err != nil {
			t.Fatal(err)
		}
		it, _ := e.read(region)
		stale := e.c.LeaseStale(it)
		e.capOps(200)
		_, err := e.c.Mark(ctx, tool, region)
		var we *lifecycle.WaitError
		switch {
		case stale && err != nil:
			t.Errorf("%s: judged stale, but the write disagreed: %v", name, err)
		case !stale && !errors.As(err, &we):
			t.Errorf("%s: judged live, but the write went through: %v", name, err)
		}
	}
}

// TestReview8SetIdleEnds (L2): a set-idle that meets a dead closer's
// sentinel it cannot clear (it keeps coming back) gives up, backing off.
func TestReview8SetIdleEnds(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	it, ok := e.read(region)
	put := func() { putSentinel(t, e, t0.Add(-time.Hour)) } // a closer that died, again and again
	put()
	cleared := 0
	e.fake().Hook = func(_ context.Context, op string) {
		if op == "DeleteItem" {
			cleared++
		}
	}
	e.onSleep = func(int) { put() }
	e.capOps(50)
	sleeps := len(e.sleeps)
	if _, err := e.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok)); !errors.Is(err, lifecycle.ErrTableClosing) {
		t.Errorf("set idle behind a sentinel that keeps coming back: %v", err)
	}
	if cleared != 2 {
		t.Errorf("%d clears, want 2", cleared)
	}
	if n := len(e.sleeps) - sleeps; n != 2 {
		t.Errorf("%d back-offs, want 2", n)
	}
}

// TestReview8ClaimRefusesALiveMark (L4): an IDLE item with a creator's live
// mark (one an older backstop's idle Observe left) is not claimed, by Claim
// or ClaimIfUnchanged.
func TestReview8ClaimRefusesALiveMark(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	if err := e.fake().PutRaw(lifecycle.TableName, map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: lifecycle.ItemKey(tool, region)}, "state": &types.AttributeValueMemberS{Value: "IDLE"},
		"idleSince": &types.AttributeValueMemberS{Value: "2026-11-15T00:00:00.000Z"}, "stampState": &types.AttributeValueMemberS{Value: "IDLE"},
		"marks": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{"creator": &types.AttributeValueMemberS{Value: "2026-11-16T11:59:00.000Z"}}},
	}); err != nil {
		t.Fatal(err)
	}
	var nc *lifecycle.NotClaimableError
	if _, err := e.c.Claim(ctx, tool, region, 0); !errors.As(err, &nc) || nc.Reason != "a creator's mark is live" {
		t.Errorf("Claim: %v", err)
	}
	it, ok := e.read(region)
	if _, err := e.c.ClaimIfUnchanged(ctx, tool, region, 0, lifecycle.ExpectOf(it, ok)); !errors.As(err, &nc) || nc.Reason != "a creator's mark is live" {
		t.Errorf("ClaimIfUnchanged: %v", err)
	}
	e.advance(time.Hour)
	if _, err := e.c.Claim(ctx, tool, region, 0); err != nil {
		t.Errorf("past the bound: %v", err)
	}
}

// TestReview8DefaultBound (L6): no bound is the default one, for the
// package functions, and the Client's own for its methods.
func TestReview8DefaultBound(t *testing.T) {
	it := lifecycle.Item{Marks: []lifecycle.MarkInfo{{Token: "a", At: t0.Add(-30 * time.Minute)}}}
	if !lifecycle.InUse(it, t0, 0) || !lifecycle.IdleSince(it, t0, -1).IsZero() {
		t.Error("the package default")
	}
	e := newEnv(t, func(o *lifecycle.Options) { o.MarkBound = 10 * time.Minute })
	if e.c.InUse(it, 0) || e.c.IdleSince(it, 0).IsZero() {
		t.Error("the Client's own bound")
	}
}

// TestReview8InUseNotChanged (L7): a set-idle of an unchanged item that has a
// hold is ErrInUse, not ErrChanged.
func TestReview8InUseNotChanged(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if err := e.c.AddHold(ctx, tool, region, "spawn"); err != nil {
		t.Fatal(err)
	}
	it, ok := e.read(region)
	if _, err := e.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok)); !errors.Is(err, lifecycle.ErrInUse) || errors.Is(err, lifecycle.ErrChanged) {
		t.Errorf("%v", err)
	}
}

// TestReview8MarkFlipsAreBounded (L1): a marks map that flips on every
// attempt is followed, but each retry counts toward the wait, and backs off
// after the first in a row: the Mark ends.
func TestReview8MarkFlipsAreBounded(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, func(o *lifecycle.Options) { o.WaitTimeout = time.Minute })
	other := e.client("other")
	tok, _ := other.Mark(ctx, tool, region)
	var hook func(ctx context.Context, op string)
	hook = func(ctx context.Context, op string) {
		if op != "TransactWriteItems" {
			return
		}
		e.fake().Hook = nil
		if it, _ := e.read(region); len(it.Marks) > 0 {
			_, _ = other.MarkDone(ctx, tool, region, tok)
			it, ok := e.read(region)
			_, _ = other.SetIdleIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok))
		} else {
			tok, _ = other.Mark(ctx, tool, region)
		}
		e.fake().Hook = hook
	}
	e.fake().Hook = hook
	e.capOps(500)
	_, err := e.c.Mark(ctx, tool, region)
	e.fake().Hook = nil
	var we *lifecycle.WaitError
	if !errors.As(err, &we) {
		t.Errorf("a Mark against a map that always flips: %v", err)
	}
	if len(e.sleeps) == 0 {
		t.Error("no back-off")
	}
}

// TestReview8SetIdleGivesUpSaying (L2): a set-idle that keeps meeting
// conflicts gives up after writeAttempts, saying why.
func TestReview8SetIdleGivesUpSaying(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	it, ok := e.read(region)
	e.fake().Err = func(op string, _ any) error {
		if op == "TransactWriteItems" {
			return &types.TransactionCanceledException{CancellationReasons: []types.CancellationReason{{}, {Code: aws.String("TransactionConflict")}}}
		}
		return nil
	}
	e.capOps(50)
	if _, err := e.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok)); err == nil || !strings.Contains(err.Error(), "gave up after") ||
		!strings.Contains(err.Error(), "conflict") {
		t.Errorf("%v", err)
	}
}
