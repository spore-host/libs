package lifecycle_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/tags"
)

// libs#88: Observe takes a dead closer's sentinel over, as activate and
// SetIdleIfUnchanged do; and a closer checks its own sentinel before it
// deletes the table.

// TestObserveClearsADeadSentinel: a closing sentinel a CloseTableIfEmpty
// that died left (older than StaleAfter) is removed, and the observation
// goes ahead at once (no back-off), active or idle, on an item that exists
// and on one that does not. Another tool's item is left as it was.
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
			if n := len(e.sleeps) - sleeps; n != 0 {
				t.Errorf("%d back-offs, want none (its own cause removed)", n)
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
// at every attempt (closers that die again and again) is cleared twice, at
// once the first time and after one back-off the second, and then Observe
// gives up with ErrTableClosing: the retry is bounded, by one PollInterval.
func TestObserveDeadSentinelComesBack(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	cleared := 0
	e.fake().Hook = func(_ context.Context, op string) {
		switch op {
		case "TransactWriteItems":
			putSentinel(t, e, t0.Add(-time.Hour))
		case "DeleteItem":
			cleared++
		}
	}
	e.capOps(50)
	sleeps := len(e.sleeps)
	if _, _, err := e.c.Observe(ctx, tool, region, false); !errors.Is(err, lifecycle.ErrTableClosing) ||
		!strings.Contains(err.Error(), "lifecycle: observe "+lifecycle.ItemKey(tool, region)) {
		t.Errorf("Observe behind a sentinel that keeps coming back: %v", err)
	}
	if cleared != 2 {
		t.Errorf("%d clears, want 2", cleared)
	}
	if n := len(e.sleeps) - sleeps; n != 1 || e.sleeps[len(e.sleeps)-1] != lifecycle.DefaultPollInterval {
		t.Errorf("%d back-offs (%v), want one PollInterval", n, e.sleeps[sleeps:])
	}
}

// TestObserveClearMeetsALiveCloser (L2): a stale sentinel that a live
// closer's replaces before the clear lands is left, and Observe returns
// ErrTableClosing at once, with no back-off.
func TestObserveClearMeetsALiveCloser(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	putSentinel(t, e, t0.Add(-time.Hour))
	e.fake().Hook = func(_ context.Context, op string) {
		if op == "DeleteItem" {
			e.fake().Hook = nil
			putSentinel(t, e, e.clock()) // a live closer, just now
		}
	}
	sleeps, writes := len(e.sleeps), e.fake().Calls("TransactWriteItems")
	if _, _, err := e.c.Observe(ctx, tool, region, false); !errors.Is(err, lifecycle.ErrTableClosing) {
		t.Errorf("Observe = %v, want ErrTableClosing", err)
	}
	if n := len(e.sleeps) - sleeps; n != 0 {
		t.Errorf("%d back-offs, want none", n)
	}
	if n := e.fake().Calls("TransactWriteItems") - writes; n != 1 {
		t.Errorf("%d observation attempts, want 1 (no retry behind a live closer)", n)
	}
	if e.fake().Item(lifecycle.TableName, lifecycle.SentinelKey) == nil {
		t.Error("the live closer's sentinel was removed")
	}
}

// TestSentinelWithNoTimeIsStale (L6): a sentinel whose since is missing, not
// a string, or not a time (it sorts outside the live window) no live closer
// wrote: Observe, MarkActive, SetIdleIfUnchanged and CloseTableIfEmpty all
// take it over. A since inside the window is a live closer's.
func TestSentinelWithNoTimeIsStale(t *testing.T) {
	ctx := context.Background()
	for name, since := range map[string]types.AttributeValue{
		"missing":              nil,
		"not a string":         &types.AttributeValueMemberN{Value: "1"},
		"not a time, early":    sAttr("0-garbage"),
		"not a time, late":     sAttr("zzz"),
		"far in the future":    sAttr(ts(t0.Add(time.Hour))),
		"older than the bound": sAttr(ts(t0.Add(-lifecycle.DefaultStaleAfter - time.Second))),
	} {
		put := func(e *env) {
			it := map[string]types.AttributeValue{"pk": sAttr(lifecycle.SentinelKey), "token": sAttr("closer")}
			if since != nil {
				it["since"] = since
			}
			if err := e.fake().PutRaw(lifecycle.TableName, it); err != nil {
				t.Fatal(err)
			}
		}
		for _, op := range []string{"Observe", "MarkActive", "SetIdleIfUnchanged", "CloseTableIfEmpty"} {
			e := newEnv(t)
			if _, _, err := e.c.Observe(ctx, tool, region, true); err != nil { // ACTIVE, with no pulse
				t.Fatal(err)
			}
			it, ok := e.read(region)
			put(e)
			e.capOps(50)
			var err error
			switch op {
			case "Observe":
				_, _, err = e.c.Observe(ctx, tool, region, false)
			case "MarkActive":
				err = e.c.MarkActive(ctx, tool, region)
			case "SetIdleIfUnchanged":
				_, err = e.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok))
			case "CloseTableIfEmpty":
				var gone bool
				gone, err = e.c.CloseTableIfEmpty(ctx, region)
				if gone {
					t.Errorf("%s: %s: the table with an item is gone", name, op)
				}
				// It took the sentinel over, found the item, and dropped its own.
				if err == nil && e.fake().Item(lifecycle.TableName, lifecycle.SentinelKey) != nil {
					t.Errorf("%s: %s: a sentinel is left", name, op)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s: %s behind it: %v", name, op, err)
			}
			if e.fake().Item(lifecycle.TableName, lifecycle.SentinelKey) != nil {
				t.Errorf("%s: %s left it", name, op)
			}
		}
	}
	// Inside the window: live, for every writer and closer.
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tags.ToolSpawn, region); err != nil {
		t.Fatal(err)
	}
	putSentinel(t, e, e.clock().Add(lifecycle.DefaultStaleAfter-time.Minute)) // a closer whose clock runs a little ahead
	if _, _, err := e.c.Observe(ctx, tool, region, false); !errors.Is(err, lifecycle.ErrTableClosing) {
		t.Errorf("a live closer ahead: %v", err)
	}
	if gone, err := e.client("closer 2").CloseTableIfEmpty(ctx, region); gone || err != nil {
		t.Errorf("a second closer: %v %v", gone, err)
	}
}

// TestObserveSentinelClearFails: a clear that fails, and a back-off cut
// short, end the Observe with their error, named.
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
	if _, _, err := e.c.Observe(ctx, tool, region, false); !errors.Is(err, boom) || !strings.Contains(err.Error(), "lifecycle: observe ") {
		t.Errorf("a clear that fails: %v", err)
	}

	stop := errors.New("stopped")
	e2 := newEnv(t, func(o *lifecycle.Options) {
		o.Sleep = func(context.Context, time.Duration) error { return stop }
	})
	if err := e2.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	e2.fake().Hook = func(_ context.Context, op string) {
		if op == "TransactWriteItems" {
			putSentinel(t, e2, t0.Add(-time.Hour)) // back every time: the second clear backs off
		}
	}
	if _, _, err := e2.c.Observe(ctx, tool, region, false); !errors.Is(err, stop) || !strings.Contains(err.Error(), "lifecycle: observe ") {
		t.Errorf("a back-off cut short: %v", err)
	}
}

// TestObserveTableWaitAfterAClear (L3): the wait for a table that is
// transitioning counts from the last clear, not from the Observe's start,
// so time spent on a dead closer's sentinel does not use the table's wait.
func TestObserveTableWaitAfterAClear(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, func(o *lifecycle.Options) {
		o.PollInterval, o.WaitTimeout, o.TablePollInterval = 10*time.Second, 15*time.Second, 2*time.Second
	})
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	writes := 0
	e.fake().Hook = func(_ context.Context, op string) {
		if op != "TransactWriteItems" {
			return
		}
		switch writes++; writes {
		case 1, 2:
			putSentinel(t, e, t0.Add(-time.Hour)) // cleared at once, then after a 10 s back-off
		case 3:
			e.fake().SetStatus(lifecycle.TableName, types.TableStatusCreating, 4) // 4 polls of 2 s
		}
	}
	e.capOps(80)
	if _, _, err := e.c.Observe(ctx, tool, region, false); err != nil {
		t.Errorf("a table wait after a clear and a back-off: %v", err)
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

// TestCloseRechecksItsSentinel (L1): just before DeleteTable a closer reads
// its sentinel again. Taken over (a writer whose clock ran far enough ahead
// took it for a dead closer's and wrote an item), it backs out and leaves
// the table and the item; replaced by another closer's, it leaves that one;
// stalled past StaleAfter/2 since it wrote it, it backs out and drops its
// own; a re-read that fails drops its own and says so.
func TestCloseRechecksItsSentinel(t *testing.T) {
	ctx := context.Background()

	// A writer ahead by more than StaleAfter takes the sentinel over during
	// the closer's scan, and writes an item.
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tags.ToolSpawn, region); err != nil {
		t.Fatal(err)
	}
	clearItems := func(e *env) {
		for _, k := range e.fake().Keys(lifecycle.TableName) {
			if k != lifecycle.SentinelKey {
				if _, err := e.fake().DeleteItem(ctx, delInput(k)); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	clearItems(e)
	ahead := e.client("writer ahead", func(o *lifecycle.Options) {
		o.Now = func() time.Time { return e.clock().Add(lifecycle.DefaultStaleAfter + time.Minute) }
	})
	e.fake().Hook = func(_ context.Context, op string) {
		if op == "GetItem" { // the closer's re-read
			e.fake().Hook = nil
			if _, _, err := ahead.Observe(ctx, tool, region, true); err != nil {
				t.Error(err)
			}
		}
	}
	if gone, err := e.c.CloseTableIfEmpty(ctx, region); gone || err != nil {
		t.Errorf("taken over: %v %v", gone, err)
	}
	if e.fake().Calls("DeleteTable") != 0 {
		t.Error("taken over: the table was deleted")
	}
	if _, ok, _ := e.c.Get(ctx, tool, region); !ok {
		t.Error("taken over: the writer's item is gone")
	}

	// Replaced by another closer's sentinel: that one stays.
	e = newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	clearItems(e)
	e.fake().Hook = func(_ context.Context, op string) {
		if op == "GetItem" {
			e.fake().Hook = nil
			putSentinel(t, e, e.clock())
		}
	}
	if gone, err := e.c.CloseTableIfEmpty(ctx, region); gone || err != nil || e.fake().Calls("DeleteTable") != 0 {
		t.Errorf("replaced: %v %v", gone, err)
	}
	if it := e.fake().Item(lifecycle.TableName, lifecycle.SentinelKey); it == nil {
		t.Error("replaced: the other closer's sentinel was dropped")
	}

	// Stalled past StaleAfter/2 between its put and the delete.
	e = newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	clearItems(e)
	e.fake().Hook = func(_ context.Context, op string) {
		if op == "Scan" {
			e.fake().Hook = nil
			e.advance(lifecycle.DefaultStaleAfter / 2)
		}
	}
	if gone, err := e.c.CloseTableIfEmpty(ctx, region); gone || err != nil || e.fake().Calls("DeleteTable") != 0 {
		t.Errorf("stalled: %v %v", gone, err)
	}
	if e.fake().Item(lifecycle.TableName, lifecycle.SentinelKey) != nil {
		t.Error("stalled: its own sentinel is left")
	}

	// A re-read that fails.
	e = newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	clearItems(e)
	boom := errors.New("boom")
	e.fake().Err = func(op string, _ any) error {
		if op == "GetItem" {
			return boom
		}
		return nil
	}
	if gone, err := e.c.CloseTableIfEmpty(ctx, region); gone || !errors.Is(err, boom) || e.fake().Calls("DeleteTable") != 0 {
		t.Errorf("a failing re-read: %v %v", gone, err)
	}
	e.fake().Err = nil
	if e.fake().Item(lifecycle.TableName, lifecycle.SentinelKey) != nil {
		t.Error("a failing re-read: its own sentinel is left")
	}

	// Unhindered: the table goes.
	e = newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	clearItems(e)
	if gone, err := e.c.CloseTableIfEmpty(ctx, region); !gone || err != nil || e.fake().Calls("DeleteTable") != 1 {
		t.Errorf("unhindered: %v %v", gone, err)
	}
}

// delInput deletes the item key from the lifecycle table, unconditionally.
func delInput(key string) *dynamodb.DeleteItemInput {
	return &dynamodb.DeleteItemInput{TableName: aws.String(lifecycle.TableName), Key: map[string]types.AttributeValue{"pk": sAttr(key)}}
}

// TestSetIdleClearMeetsALiveCloser (L2): SetIdleIfUnchanged's clear that
// finds a live closer's sentinel in place is ErrTableClosing at once, and a
// clear that fails is its error; a MarkActive's waits a poll, as for any
// blocker, rather than retrying at once.
func TestSetIdleClearMeetsALiveCloser(t *testing.T) {
	ctx := context.Background()
	for _, fail := range []bool{false, true} {
		e := newEnv(t)
		if _, _, err := e.c.Observe(ctx, tool, region, true); err != nil { // ACTIVE, with no pulse
			t.Fatal(err)
		}
		it, ok := e.read(region)
		putSentinel(t, e, t0.Add(-time.Hour))
		boom := errors.New("boom")
		e.fake().Hook = func(_ context.Context, op string) {
			if op == "DeleteItem" {
				e.fake().Hook = nil
				putSentinel(t, e, e.clock())
			}
		}
		if fail {
			e.fake().Err = func(op string, _ any) error {
				if op == "DeleteItem" {
					return boom
				}
				return nil
			}
		}
		sleeps := len(e.sleeps)
		_, err := e.c.SetIdleIfUnchanged(ctx, tool, region, lifecycle.ExpectOf(it, ok))
		switch {
		case fail && !errors.Is(err, boom):
			t.Errorf("a clear that fails: %v", err)
		case !fail && !errors.Is(err, lifecycle.ErrTableClosing):
			t.Errorf("a live closer in its place: %v", err)
		}
		if n := len(e.sleeps) - sleeps; n != 0 {
			t.Errorf("fail=%t: %d back-offs, want none", fail, n)
		}
	}

	e := newEnv(t, func(o *lifecycle.Options) { o.WaitTimeout = time.Minute })
	if err := e.c.MarkActive(ctx, tags.ToolSpawn, region); err != nil { // the table
		t.Fatal(err)
	}
	putSentinel(t, e, t0.Add(-time.Hour))
	e.fake().Hook = func(_ context.Context, op string) {
		if op == "DeleteItem" {
			e.fake().Hook = nil
			putSentinel(t, e, e.clock())
		}
	}
	e.onSleep = func(n int) {
		if n == 1 { // the live closer finishes, dropping its sentinel
			if _, err := e.fake().DeleteItem(ctx, delInput(lifecycle.SentinelKey)); err != nil {
				t.Error(err)
			}
		}
	}
	writes := e.fake().Calls("TransactWriteItems")
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Errorf("MarkActive once the live closer is done: %v", err)
	}
	if len(e.sleeps) != 1 {
		t.Errorf("%d polls, want 1 (it waited for the live closer)", len(e.sleeps))
	}
	if n := e.fake().Calls("TransactWriteItems") - writes; n != 2 {
		t.Errorf("%d attempts, want 2 (none at once behind the live closer)", n)
	}
}
