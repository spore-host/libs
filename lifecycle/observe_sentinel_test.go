package lifecycle_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/tags"
)

// libs#88: Observe takes a dead closer's sentinel over, as activate and
// SetIdleIfUnchanged do.

// TestObserveClearsADeadSentinel: a closing sentinel a CloseTableIfEmpty
// that died left (older than StaleAfter) is removed, and the observation
// goes ahead after one back-off, active or idle, on an item that exists and
// on one that does not. Another tool's item is left as it was.
func TestObserveClearsADeadSentinel(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		exists bool
		active bool
		want   lifecycle.State
	}{
		{"active, item exists", true, true, lifecycle.StateActive},
		{"idle, item exists", true, false, lifecycle.StateIdle},
		{"active, no item", false, true, lifecycle.StateActive},
		{"idle, no item", false, false, lifecycle.StateIdle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			if err := e.c.MarkActive(ctx, tags.ToolSpawn, region); err != nil { // the table holds another tool's item
				t.Fatal(err)
			}
			if tc.exists {
				if _, _, err := e.c.Observe(ctx, tool, region, true); err != nil { // ACTIVE, with no pulse
					t.Fatal(err)
				}
			}
			e.advance(time.Hour)
			putSentinel(t, e, t0) // its closer died an hour ago
			sleeps := len(e.sleeps)
			e.capOps(50)
			it, _, err := e.c.Observe(ctx, tool, region, tc.active)
			if err != nil {
				t.Fatalf("Observe behind a dead closer's sentinel: %v", err)
			}
			if it.State != tc.want {
				t.Errorf("state %s, want %s", it.State, tc.want)
			}
			if e.fake().Item(lifecycle.TableName, lifecycle.SentinelKey) != nil {
				t.Error("the dead closer's sentinel is still there")
			}
			if n := len(e.sleeps) - sleeps; n != 1 {
				t.Errorf("%d back-offs, want 1", n)
			}
			if _, ok, err := e.c.Get(ctx, tags.ToolSpawn, region); err != nil || !ok {
				t.Errorf("another tool's item: %v %v", ok, err)
			}
		})
	}
}

// TestObserveLeavesALiveCloser: a sentinel younger than StaleAfter is a
// closer still at work: Observe returns ErrTableClosing, writes nothing,
// and leaves the sentinel, at once (no back-off).
func TestObserveLeavesALiveCloser(t *testing.T) {
	ctx := context.Background()
	for _, active := range []bool{true, false} {
		e := newEnv(t)
		if err := e.c.MarkActive(ctx, tags.ToolSpawn, region); err != nil {
			t.Fatal(err)
		}
		putSentinel(t, e, e.clock().Add(-lifecycle.DefaultStaleAfter+time.Minute))
		deletes := e.fake().Calls("DeleteItem")
		sleeps := len(e.sleeps)
		if _, _, err := e.c.Observe(ctx, tool, region, active); !errors.Is(err, lifecycle.ErrTableClosing) {
			t.Errorf("active=%t: Observe behind a live closer = %v, want ErrTableClosing", active, err)
		}
		if e.fake().Item(lifecycle.TableName, lifecycle.SentinelKey) == nil {
			t.Errorf("active=%t: a live closer's sentinel was removed", active)
		}
		if n := e.fake().Calls("DeleteItem") - deletes; n != 0 {
			t.Errorf("active=%t: %d deletes, want none", active, n)
		}
		if n := len(e.sleeps) - sleeps; n != 0 {
			t.Errorf("active=%t: %d back-offs, want none", active, n)
		}
		if _, ok, _ := e.c.Get(ctx, tool, region); ok {
			t.Errorf("active=%t: Observe created an item past a live sentinel", active)
		}
	}
}

// TestObserveDeadSentinelComesBack: a dead closer's sentinel that is back
// after every clear (closers that die again and again) is cleared twice, a
// back-off after each, and then Observe gives up with ErrTableClosing: the
// retry is bounded.
func TestObserveDeadSentinelComesBack(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	put := func() { putSentinel(t, e, t0.Add(-time.Hour)) }
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
	if _, _, err := e.c.Observe(ctx, tool, region, false); !errors.Is(err, lifecycle.ErrTableClosing) {
		t.Errorf("Observe behind a sentinel that keeps coming back: %v", err)
	}
	if cleared != 2 {
		t.Errorf("%d clears, want 2", cleared)
	}
	if n := len(e.sleeps) - sleeps; n != 2 {
		t.Errorf("%d back-offs, want 2", n)
	}
}

// TestObserveSentinelWithNoTime: a sentinel with no time to age it by is
// not taken for a dead closer's (as an activation does not).
func TestObserveSentinelWithNoTime(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	if err := e.fake().PutRaw(lifecycle.TableName, map[string]types.AttributeValue{"pk": sAttr(lifecycle.SentinelKey), "token": sAttr("closer")}); err != nil {
		t.Fatal(err)
	}
	e.capOps(20)
	if _, _, err := e.c.Observe(ctx, tool, region, false); !errors.Is(err, lifecycle.ErrTableClosing) {
		t.Errorf("Observe behind a sentinel with no time = %v, want ErrTableClosing", err)
	}
	if e.fake().Item(lifecycle.TableName, lifecycle.SentinelKey) == nil {
		t.Error("a sentinel with no time was removed")
	}
}

// TestObserveSentinelClearFails: a clear that fails, and a back-off cut
// short, end the Observe with their error.
func TestObserveSentinelClearFails(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	putSentinel(t, e, t0.Add(-time.Hour))
	e.fake().Err = func(op string, _ any) error {
		if op == "DeleteItem" {
			return boom
		}
		return nil
	}
	if _, _, err := e.c.Observe(ctx, tool, region, false); !errors.Is(err, boom) {
		t.Errorf("a clear that fails: %v", err)
	}

	stop := errors.New("stopped")
	e2 := newEnv(t, func(o *lifecycle.Options) {
		o.Sleep = func(context.Context, time.Duration) error { return stop }
	})
	if err := e2.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	putSentinel(t, e2, t0.Add(-time.Hour))
	if _, _, err := e2.c.Observe(ctx, tool, region, false); !errors.Is(err, stop) {
		t.Errorf("a back-off cut short: %v", err)
	}
}

// TestObserveNoTableCannotCreate: a table that is gone and cannot be
// created ends the Observe with the create's error.
func TestObserveNoTableCannotCreate(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	e := newEnv(t)
	e.fake().Err = func(op string, _ any) error {
		if op == "CreateTable" {
			return boom
		}
		return nil
	}
	if _, _, err := e.c.Observe(ctx, tool, region, false); !errors.Is(err, boom) {
		t.Errorf("no table, and the create fails: %v", err)
	}
}
