package lastmile_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/spore-host/libs/footprint"
	"github.com/spore-host/libs/lastmile"
	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/lifecycle/lifecycletest"
	"github.com/spore-host/libs/tags"
)

const (
	east    = "us-east-1" // also the control region
	west    = "us-west-2"
	eu      = "eu-west-1"
	account = "123456789012"
	tool    = tags.ToolLagotto
	grace   = 24 * time.Hour
)

var (
	ctx = context.Background()
	t0  = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
)

// writeOps are every lifecycletest operation that changes the table.
var writeOps = []string{"PutItem", "UpdateItem", "DeleteItem", "TransactWriteItems", "CreateTable", "DeleteTable"}

// world is one test's AWS: a fake DynamoDB per region, a shared clock, a
// footprint and a counting deleter.
type world struct {
	t       *testing.T
	mu      sync.Mutex
	now     time.Time
	fakes   map[string]*lifecycletest.Fake
	backend *lifecycle.Client // another participant: the backstop, a creator
	lc      *lifecycle.Client // the CLI's client
	del     *deleter
	report  *footprint.Report
	dir     string
	logs    []string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, now: t0, fakes: map[string]*lifecycletest.Fake{east: lifecycletest.New(), west: lifecycletest.New()}, del: &deleter{}, dir: t.TempDir()}
	w.backend = w.client("backstop")
	w.lc = w.client("cli")
	w.report = footprint.NewReport(tool, account, t0)
	w.report.Resources = []footprint.Resource{
		res("arn:aws:lambda:us-west-2:123456789012:function:lagotto-poller", "AWS::Lambda::Function", west, "lagotto-poller", footprint.OwnershipTagged, footprint.ClassResidue),
		res("arn:aws:events:us-west-2:123456789012:rule/lagotto-tick", "AWS::Events::Rule", west, "lagotto-tick", footprint.OwnershipTagged, footprint.ClassResidue),
		res("arn:aws:sqs:us-west-2:123456789012:lagotto-old", "AWS::SQS::Queue", west, "lagotto-old", footprint.OwnershipLegacyName, footprint.ClassResidue),
		res("arn:aws:lambda:us-east-1:123456789012:function:lagotto-poller", "AWS::Lambda::Function", east, "lagotto-poller", footprint.OwnershipTagged, footprint.ClassResidue),
	}
	return w
}

func res(arn, typ, region, name string, own footprint.Ownership, class footprint.Class) footprint.Resource {
	r := footprint.Resource{ARN: arn, Type: typ, Region: region, Name: name, Ownership: own, Class: class, Tags: map[string]string{}}
	if own == footprint.OwnershipTagged {
		r.Tags = tags.For(tool, region, t0, "")
	}
	return r
}

// invokeRole is lagotto's shared residue.
func invokeRole() footprint.Resource {
	return res("arn:aws:iam::123456789012:role/lagotto-invoke", "AWS::IAM::Role", lifecycle.ScopeGlobal, "lagotto-invoke", footprint.OwnershipTagged, footprint.ClassResidue)
}

func (w *world) client(owner string) *lifecycle.Client {
	w.t.Helper()
	c, err := lifecycle.New(lifecycle.Options{
		DynamoDB: func(r string) (lifecycle.API, error) {
			f, ok := w.fakes[r]
			if !ok {
				return nil, errors.New("no such region " + r)
			}
			return f, nil
		},
		ControlRegion: east,
		Owner:         owner,
		Now:           w.clock,
		Sleep:         func(ctx context.Context, d time.Duration) error { w.advance(d); return ctx.Err() },
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return c
}

func (w *world) clock() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.now
}

func (w *world) advance(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.now = w.now.Add(d)
}

func (w *world) statePath() string { return filepath.Join(w.dir, ".spore-host", "lifecycle.json") }

func (w *world) opts(mod ...func(*lastmile.Options)) lastmile.Options {
	o := lastmile.Options{
		Tool:      tool,
		Account:   account,
		Regions:   []string{east, west},
		Lifecycle: w.lc,
		Footprint: func(context.Context) (*footprint.Report, error) { return w.report, nil },
		Deleter:   w.del,
		Patterns:  []footprint.NamePattern{footprint.MustPattern("", "lagotto-.*")},
		Now:       w.clock,
		StatePath: w.statePath(),
		Logf: func(format string, args ...any) {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.logs = append(w.logs, strings.TrimSpace(fmt.Sprintf(format, args...)))
		},
	}
	for _, m := range mod {
		m(&o)
	}
	return o
}

// writes counts the table writes in every region.
func (w *world) writes() int {
	n := 0
	for _, f := range w.fakes {
		for _, op := range writeOps {
			n += f.Calls(op)
		}
	}
	return n
}

// claim brings tool in scope to a claimed teardown, as a backstop would.
func (w *world) claim(scope string) *lifecycle.Lease {
	w.t.Helper()
	if err := w.backend.MarkActive(ctx, tool, scope); err != nil {
		w.t.Fatal(err)
	}
	if _, _, err := w.backend.Observe(ctx, tool, scope, false); err != nil {
		w.t.Fatal(err)
	}
	w.advance(grace)
	l, err := w.backend.Claim(ctx, tool, scope, grace)
	if err != nil {
		w.t.Fatal(err)
	}
	return l
}

// finish runs a whole teardown of scope to Finish, leaving the table empty
// of this tool's item.
func (w *world) finish(scope string) {
	w.t.Helper()
	if err := w.claim(scope).Finish(ctx); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) raw(region, scope string) map[string]types.AttributeValue {
	return w.fakes[region].Item(lifecycle.TableName, lifecycle.ItemKey(tool, scope))
}

type deleter struct {
	mu        sync.Mutex
	describes int
	deletes   int
	describe  func(footprint.Resource) (footprint.Resource, error)
}

func (d *deleter) Describe(_ context.Context, r footprint.Resource) (footprint.Resource, error) {
	d.mu.Lock()
	d.describes++
	f := d.describe
	d.mu.Unlock()
	if f != nil {
		return f(r)
	}
	return r, nil
}

func (d *deleter) Delete(context.Context, footprint.Resource) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deletes++
	return nil
}

func (d *deleter) counts() (int, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.describes, d.deletes
}

func kinds(as []lastmile.Action) map[lastmile.Kind][]string {
	m := map[lastmile.Kind][]string{}
	for _, a := range as {
		m[a.Kind] = append(m[a.Kind], a.Target)
	}
	return m
}

func has(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func readState(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Version int                       `json:"version"`
		Runs    map[string]map[string]any `json:"runs"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("state file is not JSON: %v\n%s", err, b)
	}
	if s.Version != 1 {
		t.Fatalf("state version %d", s.Version)
	}
	return s.Runs
}

func key(scope string) string { return string(tool) + "/" + account + "/" + scope }

// --- rate limit and state file ---------------------------------------------

func TestRateLimit(t *testing.T) {
	w := newWorld(t)
	if r := lastmile.Run(ctx, w.opts()); !r.Ran {
		t.Fatalf("first run skipped: %s", r.SkippedReason)
	}
	w.advance(lastmile.DefaultMinInterval - time.Minute)
	r := lastmile.Run(ctx, w.opts())
	if r.Ran || !strings.HasPrefix(r.SkippedReason, "rate-limited") {
		t.Fatalf("run inside 6 h = ran %v, %q; want rate-limited", r.Ran, r.SkippedReason)
	}
	if !strings.Contains(r.Summary(), "skipped: rate-limited") {
		t.Errorf("Summary = %q", r.Summary())
	}
	w.advance(time.Minute)
	if r := lastmile.Run(ctx, w.opts()); !r.Ran {
		t.Fatalf("run after 6 h skipped: %s", r.SkippedReason)
	}
}

func TestRateLimitIsPerToolAccountAndScope(t *testing.T) {
	w := newWorld(t)
	lastmile.Run(ctx, w.opts(func(o *lastmile.Options) { o.Regions = []string{west} }))
	// east has never run: due, although west and global are not.
	r := lastmile.Run(ctx, w.opts())
	if !r.Ran || has(r.Notes, "#"+west+":") {
		t.Fatalf("second run: ran %v, notes %v; want east only", r.Ran, r.Notes)
	}
	if !has(r.Notes, "#"+east+":") {
		t.Errorf("east not checked: %v", r.Notes)
	}
	// Another tool and another account have their own clocks.
	if r := lastmile.Run(ctx, w.opts(func(o *lastmile.Options) { o.Tool = tags.ToolSpawn })); !r.Ran {
		t.Errorf("spawn suppressed by lagotto's run: %s", r.SkippedReason)
	}
	if r := lastmile.Run(ctx, w.opts(func(o *lastmile.Options) { o.Account = "210987654321" })); !r.Ran {
		t.Errorf("another account suppressed: %s", r.SkippedReason)
	}
}

func TestStateFileFormat(t *testing.T) {
	w := newWorld(t)
	lastmile.Run(ctx, w.opts())
	fi, err := os.Stat(w.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("state file mode %v, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(w.statePath()))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("state dir mode %v, want 0700", di.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(w.statePath()))
	if len(entries) != 1 {
		t.Errorf("state dir holds %d entries, want only lifecycle.json (no temp files left)", len(entries))
	}
	runs := readState(t, w.statePath())
	for _, sc := range []string{east, west, lifecycle.ScopeGlobal} {
		rec, ok := runs[key(sc)]
		if !ok {
			t.Fatalf("no record for %s: %v", key(sc), runs)
		}
		if rec["last_run"] != t0.Format(time.RFC3339) || rec["last_attempt"] != t0.Format(time.RFC3339) {
			t.Errorf("%s = %v", sc, rec)
		}
		if _, ok := rec["incomplete"]; ok {
			t.Errorf("%s marked incomplete: %v", sc, rec)
		}
	}
}

func TestCorruptStateResets(t *testing.T) {
	for name, content := range map[string]string{
		"garbage":       "{not json",
		"wrong version": `{"version": 99, "runs": {}}`,
		"too big":       `{"version": 1, "runs": {}}` + strings.Repeat(" ", 1<<20),
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			if err := os.MkdirAll(filepath.Dir(w.statePath()), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(w.statePath(), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			r := lastmile.Run(ctx, w.opts())
			if !r.Ran {
				t.Fatalf("skipped: %s", r.SkippedReason)
			}
			if !has(r.Warnings, "reset") {
				t.Errorf("no reset warning: %v", r.Warnings)
			}
			if len(readState(t, w.statePath())) != 3 {
				t.Error("state file not rewritten")
			}
			fi, _ := os.Stat(w.statePath())
			if fi.Mode().Perm() != 0o600 {
				t.Errorf("rewritten state file mode %v, want 0600", fi.Mode().Perm())
			}
			if r := lastmile.Run(ctx, w.opts()); r.Ran {
				t.Error("rewritten state does not rate-limit")
			}
		})
	}
}

func TestUnwritableStateIsAWarning(t *testing.T) {
	w := newWorld(t)
	blocker := filepath.Join(w.dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := lastmile.Run(ctx, w.opts(func(o *lastmile.Options) { o.StatePath = filepath.Join(blocker, "x", "lifecycle.json") }))
	if !r.Ran || !has(r.Warnings, "state directory") {
		t.Errorf("ran %v, warnings %v", r.Ran, r.Warnings)
	}
}

func TestClockBackwardsIsDue(t *testing.T) {
	w := newWorld(t)
	lastmile.Run(ctx, w.opts())
	w.advance(-time.Hour)
	if r := lastmile.Run(ctx, w.opts()); !r.Ran {
		t.Errorf("a last_run in the future suppressed the run: %s", r.SkippedReason)
	}
}

// --- budget ------------------------------------------------------------------

func TestBudgetTimeoutLeavesResumableState(t *testing.T) {
	w := newWorld(t)
	stuck := true
	var mu sync.Mutex
	w.fakes[west].Hook = func(ctx context.Context, op string) {
		mu.Lock()
		s := stuck
		mu.Unlock()
		if s {
			<-ctx.Done()
		}
	}
	start := time.Now()
	r := lastmile.Run(ctx, w.opts(func(o *lastmile.Options) { o.Budget = 100 * time.Millisecond }))
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("Run took %s with a 100 ms budget", el)
	}
	if !r.Ran || !has(r.Warnings, "budget exceeded before "+west+", global") {
		t.Fatalf("ran %v, warnings %v", r.Ran, r.Warnings)
	}
	runs := readState(t, w.statePath())
	if runs[key(east)]["last_run"] == nil {
		t.Errorf("finished scope east not stamped: %v", runs)
	}
	if runs[key(west)]["last_run"] != nil || runs[key(west)]["incomplete"] != "budget exceeded" {
		t.Errorf("unfinished scope west = %v, want no last_run and incomplete", runs[key(west)])
	}
	if _, ok := runs[key(lifecycle.ScopeGlobal)]; ok {
		t.Errorf("unstarted scope global recorded: %v", runs[key(lifecycle.ScopeGlobal)])
	}

	// The next invocation, at once, picks up where this one stopped.
	mu.Lock()
	stuck = false
	mu.Unlock()
	r = lastmile.Run(ctx, w.opts())
	if !r.Ran {
		t.Fatalf("resume skipped: %s", r.SkippedReason)
	}
	if has(r.Notes, "#"+east+":") || !has(r.Notes, "#"+west+":") || !has(r.Notes, "#global:") {
		t.Errorf("resume checked the wrong scopes: %v", r.Notes)
	}
	runs = readState(t, w.statePath())
	if runs[key(west)]["last_run"] == nil || runs[key(west)]["incomplete"] != nil {
		t.Errorf("west after resume = %v", runs[key(west)])
	}
}

func TestBudgetBoundsACallbackThatIgnoresItsContext(t *testing.T) {
	w := newWorld(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	w.claim(west)
	w.advance(lifecycle.DefaultStaleAfter + time.Minute)
	start := time.Now()
	r := lastmile.Run(ctx, w.opts(func(o *lastmile.Options) {
		o.Budget = 100 * time.Millisecond
		o.Footprint = func(context.Context) (*footprint.Report, error) { <-release; return nil, nil }
	}))
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("Run took %s with a 100 ms budget", el)
	}
	if !has(r.Warnings, "budget exceeded") {
		t.Errorf("warnings %v", r.Warnings)
	}
}

func TestCancelledParent(t *testing.T) {
	w := newWorld(t)
	c, cancel := context.WithCancel(ctx)
	cancel()
	if r := lastmile.Run(c, w.opts()); r.Ran {
		t.Error("ran under a cancelled context")
	}
}

// --- stale teardowns ---------------------------------------------------------

func TestStaleLeaseIsReportedNotClaimed(t *testing.T) {
	w := newWorld(t)
	w.claim(west)
	w.advance(lifecycle.DefaultStaleAfter + time.Minute)
	before := w.raw(west, west)
	writes := w.writes()

	r := lastmile.Run(ctx, w.opts())
	k := kinds(r.WouldDo)
	itemKey := lifecycle.ItemKey(tool, west)
	if !reflect.DeepEqual(k[lastmile.KindResumeTeardown], []string{itemKey}) {
		t.Fatalf("resume = %v, want [%s]; would %v", k[lastmile.KindResumeTeardown], itemKey, r.WouldDo)
	}
	for _, a := range r.WouldDo {
		if a.Kind == lastmile.KindResumeTeardown && (!strings.Contains(a.Detail, "stale") || !strings.Contains(a.Detail, "backstop")) {
			t.Errorf("resume detail %q", a.Detail)
		}
	}
	// The plan: the two tagged west resources, not the legacy-name one, not
	// east's.
	wantDel := []string{
		"arn:aws:lambda:us-west-2:123456789012:function:lagotto-poller",
		"arn:aws:events:us-west-2:123456789012:rule/lagotto-tick",
	}
	if !reflect.DeepEqual(k[lastmile.KindDelete], wantDel) {
		t.Errorf("deletes = %v, want %v", k[lastmile.KindDelete], wantDel)
	}
	if !has(r.Notes, "lagotto-old: kept") {
		t.Errorf("legacy-name resource not noted as kept: %v", r.Notes)
	}
	if !reflect.DeepEqual(w.raw(west, west), before) {
		t.Errorf("item changed:\n before %v\n after  %v", before, w.raw(west, west))
	}
	if got := w.writes(); got != writes {
		t.Errorf("report mode made %d lifecycle writes", got-writes)
	}
	if desc, del := w.del.counts(); del != 0 || desc == 0 {
		t.Errorf("deleter: %d describes, %d deletes; want some describes and no delete", desc, del)
	}
	if len(r.Did) != 0 {
		t.Errorf("Did = %v", r.Did)
	}
	if !strings.Contains(r.Summary(), "lastmile REPORT tool=lagotto account=123456789012 would=3") {
		t.Errorf("Summary = %q", r.Summary())
	}
	if !has(w.logs, "would resume teardown of "+itemKey) {
		t.Errorf("would-action not logged: %v", w.logs)
	}
}

// TestStaleTeardownIsNotDirectlyClaimable pins the lifecycle behaviour the
// package doc's libs#79 note describes: Claim refuses a TEARDOWN item even
// when its lease is stale, so reap mode cannot resume a teardown with Claim
// alone. If this starts failing, lifecycle grew a takeover path: update the
// note.
func TestStaleTeardownIsNotDirectlyClaimable(t *testing.T) {
	w := newWorld(t)
	w.claim(west)
	w.advance(lifecycle.DefaultStaleAfter + time.Minute)
	if _, err := w.client("resumer").Claim(ctx, tool, west, grace); !errors.Is(err, lifecycle.ErrNotClaimable) {
		t.Errorf("Claim of a stale TEARDOWN item = %v, want ErrNotClaimable", err)
	}
}

func TestLiveLeaseIsLeftAlone(t *testing.T) {
	w := newWorld(t)
	w.claim(west)
	w.advance(lifecycle.DefaultStaleAfter) // exactly 20 min: still live
	r := lastmile.Run(ctx, w.opts())
	if len(kinds(r.WouldDo)[lastmile.KindResumeTeardown]) != 0 {
		t.Errorf("resumed a live teardown: %v", r.WouldDo)
	}
	if !has(r.Notes, "teardown in progress by backstop") {
		t.Errorf("notes %v", r.Notes)
	}
}

// --- residue -----------------------------------------------------------------

func TestResidueOnlyWhenNoLeaseItemsRemain(t *testing.T) {
	w := newWorld(t)
	residue := func(o *lastmile.Options) { o.Residue = []footprint.Resource{invokeRole()} }

	if err := w.backend.MarkActive(ctx, tool, west); err != nil {
		t.Fatal(err)
	}
	r := lastmile.Run(ctx, w.opts(residue))
	if has(kinds(r.WouldDo)[lastmile.KindDelete], "lagotto-invoke") {
		t.Fatalf("residue reported while %s remains: %v", lifecycle.ItemKey(tool, west), r.WouldDo)
	}
	if !has(r.Notes, "residue kept: "+lifecycle.ItemKey(tool, west)+" remains") {
		t.Errorf("notes %v", r.Notes)
	}

	// The regional teardown finishes; the item is gone from every scope.
	w.finish(west)
	w.advance(lastmile.DefaultMinInterval)
	r = lastmile.Run(ctx, w.opts(residue))
	if _, del := w.del.counts(); del != 0 {
		t.Errorf("%d Delete calls", del)
	}
	k := kinds(r.WouldDo)
	if !reflect.DeepEqual(k[lastmile.KindDelete], []string{invokeRole().ARN}) {
		t.Errorf("deletes = %v, want the invoke role", k[lastmile.KindDelete])
	}
	if !reflect.DeepEqual(k[lastmile.KindCloseTable], []string{lifecycle.TableName}) {
		t.Errorf("close-table = %v, want one (west's table is empty)", r.WouldDo)
	}
	for _, a := range r.WouldDo {
		if a.Kind == lastmile.KindCloseTable && a.Region != west {
			t.Errorf("close-table in %s, want %s", a.Region, west)
		}
	}
}

func TestResidueWaitsForAGlobalItem(t *testing.T) {
	w := newWorld(t)
	if err := w.backend.MarkActive(ctx, tool, lifecycle.ScopeGlobal); err != nil {
		t.Fatal(err)
	}
	r := lastmile.Run(ctx, w.opts(func(o *lastmile.Options) { o.Residue = []footprint.Resource{invokeRole()} }))
	if len(kinds(r.WouldDo)[lastmile.KindDelete]) != 0 {
		t.Errorf("residue reported while the global item remains: %v", r.WouldDo)
	}
}

func TestTableWithAnotherToolsItemIsKept(t *testing.T) {
	w := newWorld(t)
	if err := w.backend.MarkActive(ctx, tags.ToolSpawn, west); err != nil {
		t.Fatal(err)
	}
	r := lastmile.Run(ctx, w.opts())
	if len(kinds(r.WouldDo)[lastmile.KindCloseTable]) != 0 {
		t.Errorf("would close a table spawn still uses: %v", r.WouldDo)
	}
	if !has(r.Notes, "holds 1 tool item(s); kept") {
		t.Errorf("notes %v", r.Notes)
	}
}

func TestResidueNeedsAControlRegion(t *testing.T) {
	w := newWorld(t)
	c, err := lifecycle.New(lifecycle.Options{DynamoDB: func(r string) (lifecycle.API, error) { return w.fakes[r], nil }, Now: w.clock})
	if err != nil {
		t.Fatal(err)
	}
	r := lastmile.Run(ctx, w.opts(func(o *lastmile.Options) {
		o.Lifecycle = c
		o.Residue = []footprint.Resource{invokeRole()}
	}))
	if !r.Ran || len(r.WouldDo) != 0 || !has(r.Warnings, "residue not judged") {
		t.Errorf("ran %v, would %v, warnings %v", r.Ran, r.WouldDo, r.Warnings)
	}
}

// --- idle check --------------------------------------------------------------

func TestFullVisibilityFalseSkipsIdleCheck(t *testing.T) {
	w := newWorld(t)
	called := 0
	r := lastmile.Run(ctx, w.opts(func(o *lastmile.Options) {
		o.Activity = func(context.Context, string) (bool, error) { called++; return false, nil }
	}))
	if called != 0 {
		t.Errorf("Activity called %d times with FullVisibility false", called)
	}
	if !has(r.Notes, "idle check skipped") || !has(r.Notes, "FullVisibility is false") {
		t.Errorf("no reason given: %v", r.Notes)
	}
}

func TestIdleCheck(t *testing.T) {
	visible := func(active bool, err error) func(*lastmile.Options) {
		return func(o *lastmile.Options) {
			o.Tool = tags.ToolSpawn
			o.FullVisibility = true
			o.Activity = func(_ context.Context, sc string) (bool, error) { return active && sc == west, err }
			o.Patterns = []footprint.NamePattern{footprint.MustPattern("", "spawn-.*")}
		}
	}
	setup := func(t *testing.T, idleFor time.Duration) *world {
		w := newWorld(t)
		w.report = footprint.NewReport(tags.ToolSpawn, account, t0)
		r := res("arn:aws:ec2:us-west-2:123456789012:security-group/sg-1", "AWS::EC2::SecurityGroup", west, "spawn-sg", footprint.OwnershipTagged, footprint.ClassResidue)
		r.Tags = tags.For(tags.ToolSpawn, west, t0, "")
		w.report.Resources = []footprint.Resource{r}
		if err := w.backend.MarkActive(ctx, tags.ToolSpawn, west); err != nil {
			t.Fatal(err)
		}
		if idleFor > 0 {
			if _, _, err := w.backend.Observe(ctx, tags.ToolSpawn, west, false); err != nil {
				t.Fatal(err)
			}
			w.advance(idleFor)
		}
		return w
	}
	idleKey := lifecycle.ItemKey(tags.ToolSpawn, west)

	t.Run("idle past the grace", func(t *testing.T) {
		w := setup(t, grace+time.Hour)
		writes := w.writes()
		r := lastmile.Run(ctx, w.opts(visible(false, nil)))
		k := kinds(r.WouldDo)
		if !reflect.DeepEqual(k[lastmile.KindIdleTeardown], []string{idleKey}) || len(k[lastmile.KindDelete]) != 1 {
			t.Errorf("would %v", r.WouldDo)
		}
		if w.writes() != writes {
			t.Error("the idle check wrote to the lifecycle table (Observe or Claim)")
		}
	})
	t.Run("idle inside the grace", func(t *testing.T) {
		w := setup(t, time.Hour)
		r := lastmile.Run(ctx, w.opts(visible(false, nil)))
		if len(r.WouldDo) != 0 || !has(r.Notes, "idle for 1h0m0s of the 24h0m0s grace") {
			t.Errorf("would %v, notes %v", r.WouldDo, r.Notes)
		}
	})
	t.Run("never observed idle", func(t *testing.T) {
		w := setup(t, 0)
		r := lastmile.Run(ctx, w.opts(visible(false, nil)))
		if len(r.WouldDo) != 0 || !has(r.Notes, "Observe would start the 24h0m0s grace") {
			t.Errorf("would %v, notes %v", r.WouldDo, r.Notes)
		}
	})
	t.Run("active", func(t *testing.T) {
		w := setup(t, grace+time.Hour)
		r := lastmile.Run(ctx, w.opts(visible(true, nil)))
		if len(kinds(r.WouldDo)[lastmile.KindIdleTeardown]) != 0 || !has(r.Notes, idleKey+": active") {
			t.Errorf("would %v, notes %v", r.WouldDo, r.Notes)
		}
	})
	t.Run("failed read counts as active", func(t *testing.T) {
		w := setup(t, grace+time.Hour)
		r := lastmile.Run(ctx, w.opts(visible(false, errors.New("ec2: AccessDenied"))))
		if len(kinds(r.WouldDo)[lastmile.KindIdleTeardown]) != 0 || !has(r.Warnings, "counted as active: ec2: AccessDenied") {
			t.Errorf("would %v, warnings %v", r.WouldDo, r.Warnings)
		}
	})
	t.Run("hold counts as active", func(t *testing.T) {
		w := setup(t, 0)
		if err := w.backend.AddHold(ctx, tags.ToolSpawn, west, tags.ToolCalque); err != nil {
			t.Fatal(err)
		}
		r := lastmile.Run(ctx, w.opts(visible(false, nil)))
		if len(r.WouldDo) != 0 || !has(r.Notes, "held by [calque]") {
			t.Errorf("would %v, notes %v", r.WouldDo, r.Notes)
		}
	})
	t.Run("no item and nothing in the footprint", func(t *testing.T) {
		w := newWorld(t)
		w.report = footprint.NewReport(tags.ToolSpawn, account, t0)
		r := lastmile.Run(ctx, w.opts(visible(false, nil)))
		if len(r.WouldDo) != 0 || !has(r.Notes, "no lifecycle item and nothing in the footprint") {
			t.Errorf("would %v, notes %v", r.WouldDo, r.Notes)
		}
	})
	t.Run("no Activity", func(t *testing.T) {
		w := setup(t, grace+time.Hour)
		r := lastmile.Run(ctx, w.opts(visible(false, nil), func(o *lastmile.Options) { o.Activity = nil }))
		if len(r.WouldDo) != 0 || !has(r.Warnings, "Activity is nil") {
			t.Errorf("would %v, warnings %v", r.WouldDo, r.Warnings)
		}
	})
}

// --- report mode changes nothing ----------------------------------------------

// TestReportModeChangesNothing drives every path that would change something
// (a stale teardown, an idle tool past its grace, residue, an empty table)
// in one run, in report mode and with reap requested, and checks that not one
// lifecycle write or Delete call was made.
func TestReportModeChangesNothing(t *testing.T) {
	for _, mode := range []lastmile.Mode{"", lastmile.ModeReport, lastmile.ModeReap} {
		t.Run(string(mode), func(t *testing.T) {
			w := newWorld(t)
			w.report.Resources = append(w.report.Resources, invokeRole())
			if err := w.backend.MarkActive(ctx, tool, lifecycle.ScopeGlobal); err != nil {
				t.Fatal(err)
			}
			if _, _, err := w.backend.Observe(ctx, tool, lifecycle.ScopeGlobal, false); err != nil { // global goes idle
				t.Fatal(err)
			}
			w.claim(west)                                        // a teardown...
			w.advance(lifecycle.DefaultStaleAfter + time.Minute) // ...gone stale
			w.fakes[eu] = lifecycletest.New()
			w.finish(eu) // eu's table is empty
			w.advance(time.Minute)
			snapshot := map[string][]string{}
			for reg, f := range w.fakes {
				snapshot[reg] = f.Keys(lifecycle.TableName)
			}
			before := w.raw(west, west)
			writes := w.writes()

			r := lastmile.Run(ctx, w.opts(func(o *lastmile.Options) {
				o.Mode = mode
				o.Regions = []string{east, west, eu}
				o.FullVisibility = true
				o.Activity = func(context.Context, string) (bool, error) { return false, nil }
				o.Residue = []footprint.Resource{invokeRole()}
			}))
			if r.Mode != lastmile.ModeReport {
				t.Errorf("Result.Mode = %q", r.Mode)
			}
			if mode == lastmile.ModeReap && !has(r.Warnings, "libs#79") {
				t.Errorf("reap request not warned about: %v", r.Warnings)
			}
			k := kinds(r.WouldDo)
			if len(k[lastmile.KindResumeTeardown]) != 1 || len(k[lastmile.KindIdleTeardown]) != 1 || len(k[lastmile.KindCloseTable]) != 1 || len(k[lastmile.KindDelete]) != 3 {
				t.Errorf("would %v", r.WouldDo)
			}
			if got := w.writes(); got != writes {
				for reg, f := range w.fakes {
					for _, op := range writeOps {
						t.Logf("%s %s: %d", reg, op, f.Calls(op))
					}
				}
				t.Errorf("report mode made %d lifecycle writes", got-writes)
			}
			if _, del := w.del.counts(); del != 0 {
				t.Errorf("report mode made %d Delete calls", del)
			}
			if len(r.Did) != 0 {
				t.Errorf("Did = %v", r.Did)
			}
			if !reflect.DeepEqual(w.raw(west, west), before) {
				t.Error("the stale item changed")
			}
			for reg, keys := range snapshot {
				if got := w.fakes[reg].Keys(lifecycle.TableName); !reflect.DeepEqual(got, keys) {
					t.Errorf("%s items %v, want %v", reg, got, keys)
				}
				if w.fakes[reg].Status(lifecycle.TableName) == "" {
					t.Errorf("%s table deleted", reg)
				}
			}
		})
	}
}

func TestNoDeleterPlansAsListed(t *testing.T) {
	w := newWorld(t)
	w.claim(west)
	w.advance(lifecycle.DefaultStaleAfter + time.Minute)
	r := lastmile.Run(ctx, w.opts(func(o *lastmile.Options) { o.Deleter = nil }))
	n := 0
	for _, a := range r.WouldDo {
		if a.Kind == lastmile.KindDelete {
			n++
			if !strings.Contains(a.Detail, "not re-checked") {
				t.Errorf("detail %q", a.Detail)
			}
		}
	}
	if n != 2 {
		t.Errorf("%d deletes, want 2: %v", n, r.WouldDo)
	}
}

// --- errors ------------------------------------------------------------------

func TestErrorsAreWarnings(t *testing.T) {
	stale := func(w *world) {
		w.claim(west)
		w.advance(lifecycle.DefaultStaleAfter + time.Minute)
	}
	cases := map[string]struct {
		setup func(*world)
		mod   func(*world, *lastmile.Options)
		want  string
	}{
		"dynamodb down": {
			mod: func(w *world, _ *lastmile.Options) {
				for _, f := range w.fakes {
					f.Err = func(string, any) error { return errors.New("connection reset") }
				}
			},
			want: "connection reset",
		},
		"footprint error": {
			setup: stale,
			mod: func(_ *world, o *lastmile.Options) {
				o.Footprint = func(context.Context) (*footprint.Report, error) { return nil, errors.New("ec2 throttled") }
			},
			want: "footprint: ec2 throttled",
		},
		"footprint panic": {
			setup: stale,
			mod: func(_ *world, o *lastmile.Options) {
				o.Footprint = func(context.Context) (*footprint.Report, error) { panic("nil map") }
			},
			want: "panic: nil map",
		},
		"footprint nil": {
			setup: stale,
			mod: func(_ *world, o *lastmile.Options) {
				o.Footprint = func(context.Context) (*footprint.Report, error) { return nil, nil }
			},
			want: "footprint: no report",
		},
		"footprint for another account": {
			setup: stale,
			mod:   func(w *world, _ *lastmile.Options) { w.report.Account = "210987654321" },
			want:  "ignored",
		},
		"footprint warnings": {
			setup: stale,
			mod:   func(w *world, _ *lastmile.Options) { w.report.Warnings = []string{"eu-west-1 unreachable"} },
			want:  "footprint: eu-west-1 unreachable",
		},
		"describe fails": {
			setup: stale,
			mod: func(w *world, _ *lastmile.Options) {
				w.del.describe = func(footprint.Resource) (footprint.Resource, error) {
					return footprint.Resource{}, errors.New("AccessDenied")
				}
			},
			want: "AccessDenied",
		},
		"describe panics": {
			setup: stale,
			mod: func(w *world, _ *lastmile.Options) {
				w.del.describe = func(footprint.Resource) (footprint.Resource, error) { panic("boom") }
			},
			want: "panic: boom",
		},
		"activity panics": {
			mod: func(_ *world, o *lastmile.Options) {
				o.FullVisibility = true
				o.Activity = func(context.Context, string) (bool, error) { panic("signal reader") }
			},
			want: "counted as active: panic: signal reader",
		},
		"logger panics": {
			setup: stale,
			mod: func(_ *world, o *lastmile.Options) {
				o.Logf = func(string, ...any) { panic("closed log") }
			},
			want: "internal error (recovered): closed log",
		},
		"dependency cycle": {
			setup: stale,
			mod: func(w *world, _ *lastmile.Options) {
				a, b := &w.report.Resources[0], &w.report.Resources[1]
				a.DependsOn, b.DependsOn = []string{b.ARN}, []string{a.ARN}
			},
			want: "dependency cycle",
		},
		"bad region": {
			mod:  func(_ *world, o *lastmile.Options) { o.Regions = append(o.Regions, "", "global") },
			want: `ignoring region ""`,
		},
		"unknown mode": {
			mod:  func(_ *world, o *lastmile.Options) { o.Mode = "yolo" },
			want: `unknown mode "yolo"`,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			if c.setup != nil {
				c.setup(w)
			}
			o := w.opts()
			c.mod(w, &o)
			var r lastmile.Result
			func() {
				defer func() {
					if p := recover(); p != nil {
						t.Fatalf("Run panicked: %v", p)
					}
				}()
				r = lastmile.Run(ctx, o)
			}()
			if !has(r.Warnings, c.want) {
				t.Errorf("warnings %v, want one containing %q", r.Warnings, c.want)
			}
			if _, del := w.del.counts(); del != 0 {
				t.Errorf("%d Delete calls", del)
			}
		})
	}
}

func TestMisconfigured(t *testing.T) {
	w := newWorld(t)
	r := lastmile.Run(ctx, lastmile.Options{StatePath: w.statePath()})
	if r.Ran || !strings.Contains(r.SkippedReason, "Tool, Account, Regions, Lifecycle missing") || len(r.Warnings) == 0 {
		t.Errorf("ran %v, skipped %q, warnings %v", r.Ran, r.SkippedReason, r.Warnings)
	}
	if _, err := os.Stat(w.statePath()); !os.IsNotExist(err) {
		t.Error("a misconfigured run wrote the state file")
	}
}

func TestModeOff(t *testing.T) {
	w := newWorld(t)
	r := lastmile.Run(ctx, w.opts(func(o *lastmile.Options) { o.Mode = lastmile.ModeOff }))
	if r.Ran || !strings.Contains(r.SkippedReason, "disabled") || w.fakes[east].Calls("GetItem") != 0 {
		t.Errorf("off: ran %v, %q", r.Ran, r.SkippedReason)
	}
	if got := r.Summary(); got != "lastmile OFF tool=lagotto account=123456789012 skipped: disabled (SPORE_LASTMILE=off)" {
		t.Errorf("Summary = %q", got)
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]lastmile.Mode{"": lastmile.ModeReport, "report": lastmile.ModeReport, " OFF ": lastmile.ModeOff, "Reap": lastmile.ModeReap} {
		if got, err := lastmile.ParseMode(in); err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if got, err := lastmile.ParseMode("delete-everything"); err == nil || got != lastmile.ModeReport {
		t.Errorf("ParseMode(unknown) = %q, %v; want report and an error", got, err)
	}
	t.Setenv(lastmile.EnvVar, "off")
	if lastmile.ModeFromEnv() != lastmile.ModeOff {
		t.Error("ModeFromEnv ignored off")
	}
	t.Setenv(lastmile.EnvVar, "bogus")
	if lastmile.ModeFromEnv() != lastmile.ModeReport {
		t.Error("ModeFromEnv(unknown) is not report")
	}
}

func TestDefaultStatePath(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	p, err := lastmile.DefaultStatePath()
	if err != nil || p != filepath.Join("/home/u", ".spore-host", "lifecycle.json") {
		t.Errorf("DefaultStatePath = %q, %v", p, err)
	}
}

func TestActionString(t *testing.T) {
	for a, want := range map[lastmile.Action]string{
		{Kind: lastmile.KindResumeTeardown, Target: "tool#x#r", Detail: "stale"}: "resume teardown of tool#x#r (stale)",
		{Kind: lastmile.KindIdleTeardown, Target: "tool#x#r"}:                    "claim and tear down tool#x#r",
		{Kind: lastmile.KindDelete, Type: "AWS::IAM::Role", Target: "arn"}:       "delete AWS::IAM::Role arn",
		{Kind: lastmile.KindCloseTable, Target: "t", Region: "r"}:                "close table t in r",
		{Kind: "other", Target: "x"}:                                             "other x",
	} {
		if got := a.String(); got != want {
			t.Errorf("String = %q, want %q", got, want)
		}
	}
}

// TestStaleLeaseOnIdleItemIsNotClaimed covers the stale lease Claim would
// take over (an IDLE item, idle past the grace, still carrying a dead
// holder's lease): report mode must report it and leave it.
func TestStaleLeaseOnIdleItemIsNotClaimed(t *testing.T) {
	w := newWorld(t)
	if err := w.backend.MarkActive(ctx, tool, west); err != nil {
		t.Fatal(err)
	}
	s := func(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }
	ts := func(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
	if err := w.fakes[west].PutRaw(lifecycle.TableName, map[string]types.AttributeValue{
		"pk":        s(lifecycle.ItemKey(tool, west)),
		"tool":      s(string(tool)),
		"scope":     s(west),
		"state":     s(string(lifecycle.StateIdle)),
		"epoch":     &types.AttributeValueMemberN{Value: "1"},
		"idleSince": s(ts(t0.Add(-2 * grace))),
		"lease": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
			"token": s("dead"), "owner": s("dead-backstop"), "heartbeat": s(ts(t0.Add(-time.Hour))), "step": s("delete AWS::Lambda::Function x"),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	before, writes := w.raw(west, west), w.writes()
	r := lastmile.Run(ctx, w.opts())
	if len(kinds(r.WouldDo)[lastmile.KindResumeTeardown]) != 1 {
		t.Fatalf("would %v", r.WouldDo)
	}
	for _, a := range r.WouldDo {
		if a.Kind == lastmile.KindResumeTeardown && !strings.Contains(a.Detail, `at step "delete AWS::Lambda::Function x"`) {
			t.Errorf("detail %q", a.Detail)
		}
	}
	if w.writes() != writes || !reflect.DeepEqual(w.raw(west, west), before) {
		t.Error("report mode changed the stale item (claimed it?)")
	}
	// Claim would have taken it: the lease is the one a reap run would take
	// over.
	if _, err := w.client("check").Claim(ctx, tool, west, grace); err != nil {
		t.Errorf("the seeded item is not claimable, so this test proves nothing: %v", err)
	}
}

func TestNoFootprintStillReportsTheResume(t *testing.T) {
	w := newWorld(t)
	w.claim(west)
	w.advance(lifecycle.DefaultStaleAfter + time.Minute)
	r := lastmile.Run(ctx, w.opts(func(o *lastmile.Options) { o.Footprint = nil }))
	if k := kinds(r.WouldDo); len(k[lastmile.KindResumeTeardown]) != 1 || len(k[lastmile.KindDelete]) != 0 {
		t.Errorf("would %v", r.WouldDo)
	}
	if !has(r.Notes, "no Footprint") {
		t.Errorf("notes %v", r.Notes)
	}
}
