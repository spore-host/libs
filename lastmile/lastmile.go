package lastmile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spore-host/libs/footprint"
	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/tags"
)

// Defaults for Options.
const (
	DefaultBudget      = 30 * time.Second
	DefaultMinInterval = 6 * time.Hour
	DefaultGrace       = 24 * time.Hour
)

// EnvVar is the environment variable that switches the last mile: off,
// report or reap. Unset means report.
const EnvVar = "SPORE_LASTMILE"

// Mode is how Run acts on what it finds.
type Mode string

// Modes. The zero Mode is ModeReport.
const (
	// ModeOff: Run does nothing.
	ModeOff Mode = "off"
	// ModeReport: Run reports what it would do and changes nothing.
	ModeReport Mode = "report"
	// ModeReap: reserved for libs#79. In this release it runs as ModeReport,
	// with a warning.
	ModeReap Mode = "reap"
)

// ParseMode parses an EnvVar value. Empty is ModeReport; case and
// surrounding space are ignored.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case "":
		return ModeReport, nil
	case ModeOff, ModeReport, ModeReap:
		return m, nil
	default:
		return ModeReport, fmt.Errorf("lastmile: %s=%q: want off, report or reap", EnvVar, s)
	}
}

// ModeFromEnv returns the mode EnvVar selects. An unset or unknown value is
// ModeReport, which changes nothing.
func ModeFromEnv() Mode {
	m, _ := ParseMode(os.Getenv(EnvVar))
	return m
}

// Options configures Run. Tool, Account, Regions and Lifecycle are required;
// without them Run skips, with a warning.
type Options struct {
	// Tool is the tool whose leftovers to check.
	Tool tags.Tool
	// Account is the AWS account the caller's credentials are in.
	Account string
	// Regions are every region the tool may have resources or lifecycle items
	// in. The residue check reads the tool's item in each of them.
	Regions []string

	// Lifecycle reads the lease table. Set its ControlRegion, or the global
	// scope and the residue check are skipped.
	Lifecycle *lifecycle.Client

	// Footprint is the tool's own inventory (the function behind
	// `<tool> footprint -o json`). Run calls it at most once, and only if a
	// scope needs a plan. Its Report must be for Tool and Account.
	Footprint func(ctx context.Context) (*footprint.Report, error)
	// Deleter re-reads resources while planning. Run calls only Describe.
	// Without one, plans are made from the Footprint as it stands, and each
	// would-action says it was not re-checked.
	Deleter footprint.Deleter
	// Patterns are the tool's resource name patterns (footprint.Deletable).
	Patterns []footprint.NamePattern
	// Residue are resources that outlive every scope's teardown (lagotto's
	// shared invoke role, ...). They are judged removable only once no
	// lifecycle item of Tool remains in any scope.
	Residue []footprint.Resource

	// FullVisibility says that Activity reads every one of the tool's
	// activity signals, so Run may judge the tool idle itself. Set it only
	// for a tool with no backstop. When false, the idle check is skipped.
	FullVisibility bool
	// Activity reports whether any of the tool's activity signals in scope
	// (a region or lifecycle.ScopeGlobal) is live. An error counts as
	// active. Required when FullVisibility is set.
	Activity func(ctx context.Context, scope string) (bool, error)

	// Mode is ModeReport if empty.
	Mode Mode
	// Grace is how long a tool must be idle before teardown. Default 24 h.
	Grace time.Duration

	// Now is the clock for the rate limit and the grace. Default time.Now.
	// (Lease staleness is the lifecycle Client's call, on its own clock.)
	Now func() time.Time
	// StatePath is the rate-limit state file. Default DefaultStatePath().
	StatePath string
	// Budget bounds the whole run. Default 30 s.
	Budget time.Duration
	// MinInterval is how often each scope is checked at most. Default 6 h.
	MinInterval time.Duration

	// Logf receives one line per would-action, note and warning. Default:
	// discard.
	Logf func(format string, args ...any)
}

// Kind is what a would-action does.
type Kind string

// Kinds of Action.
const (
	// KindResumeTeardown: take over a stale teardown lease and continue its
	// plan.
	KindResumeTeardown Kind = "resume-teardown"
	// KindIdleTeardown: claim a tool idle for the grace and tear it down.
	KindIdleTeardown Kind = "idle-teardown"
	// KindDelete: delete one resource, as part of a teardown or as residue.
	KindDelete Kind = "delete"
	// KindCloseTable: delete an empty spore-host-lifecycle table
	// (lifecycle.Client.CloseTableIfEmpty).
	KindCloseTable Kind = "close-table"
)

// Action is one thing Run would do (or, in reap mode, did).
type Action struct {
	Kind Kind `json:"kind"`
	// Scope is the lifecycle scope it belongs to (a region or "global").
	Scope string `json:"scope,omitempty"`
	// Region is where the target is: a region, or "global" for IAM.
	Region string `json:"region,omitempty"`
	// Target is the lifecycle item key, the resource ARN or the table name.
	Target string `json:"target"`
	// Type and Name describe a resource, for KindDelete.
	Type string `json:"type,omitempty"`
	Name string `json:"name,omitempty"`
	// Detail says why.
	Detail string `json:"detail,omitempty"`
}

func (a Action) String() string {
	var s string
	switch a.Kind {
	case KindResumeTeardown:
		s = "resume teardown of " + a.Target
	case KindIdleTeardown:
		s = "claim and tear down " + a.Target
	case KindDelete:
		s = "delete " + a.Type + " " + a.Target
	case KindCloseTable:
		s = "close table " + a.Target + " in " + a.Region
	default:
		s = string(a.Kind) + " " + a.Target
	}
	if a.Detail != "" {
		s += " (" + a.Detail + ")"
	}
	return s
}

// Result is what Run found and did.
type Result struct {
	Mode    Mode      `json:"mode"`
	Tool    tags.Tool `json:"tool"`
	Account string    `json:"account"`
	// Ran is false when Run did nothing: disabled, rate-limited or
	// misconfigured. SkippedReason says which.
	Ran           bool   `json:"ran"`
	SkippedReason string `json:"skipped_reason,omitempty"`
	// WouldDo is every action Run would take in reap mode.
	WouldDo []Action `json:"would_do"`
	// Did is every action Run took. Always empty in report mode.
	Did []Action `json:"did"`
	// Notes are what Run checked and left alone, and why.
	Notes []string `json:"notes"`
	// Warnings are every error Run met. None of them is fatal.
	Warnings []string      `json:"warnings"`
	Duration time.Duration `json:"duration"`
}

// Summary is a one-line account of the run, for a debug or verbose log.
func (r Result) Summary() string {
	mode := strings.ToUpper(string(r.Mode))
	if mode == "" {
		mode = strings.ToUpper(string(ModeReport))
	}
	s := fmt.Sprintf("lastmile %s tool=%s account=%s", mode, r.Tool, r.Account)
	if !r.Ran {
		return s + " skipped: " + r.SkippedReason
	}
	s += fmt.Sprintf(" would=%d", len(r.WouldDo))
	if len(r.Did) > 0 {
		s += fmt.Sprintf(" did=%d", len(r.Did))
	}
	if len(r.Warnings) > 0 {
		s += fmt.Sprintf(" warnings=%d", len(r.Warnings))
	}
	return s + fmt.Sprintf(" in %s", r.Duration.Round(time.Millisecond))
}

// errBudget is the cause of the budget context.
var errBudget = errors.New("lastmile: time budget spent")

// Run checks the tool's leftovers in Account (see the package doc) and
// reports what it would do. It never returns an error and never panics:
// every problem is a line in Result.Warnings. It returns within about
// Options.Budget.
func Run(ctx context.Context, o Options) (res Result) {
	start := time.Now()
	r := &runner{o: o, res: &res}
	defer func() {
		if p := recover(); p != nil {
			// Not r.warn: the panic may have come from Logf.
			res.Warnings = append(res.Warnings, fmt.Sprintf("lastmile: internal error (recovered): %v", p))
		}
		res.Duration = time.Since(start)
	}()
	res.Tool, res.Account = o.Tool, o.Account
	r.run(ctx)
	return res
}

type runner struct {
	o   Options
	res *Result

	footprintDone bool
	footprint     *footprint.Report
	tablesChecked map[string]bool
}

func (r *runner) logf(format string, args ...any) {
	if r.o.Logf != nil {
		r.o.Logf(format, args...)
	}
}

func (r *runner) warn(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	r.res.Warnings = append(r.res.Warnings, msg)
	r.logf("lastmile: warning: %s", msg)
}

func (r *runner) note(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	r.res.Notes = append(r.res.Notes, msg)
	r.logf("lastmile: %s", msg)
}

func (r *runner) would(a Action) {
	r.res.WouldDo = append(r.res.WouldDo, a)
	r.logf("lastmile: would %s", a)
}

func (r *runner) skip(why string) {
	r.res.SkippedReason = why
	r.logf("lastmile: skipped: %s", why)
}

func (r *runner) now() time.Time {
	if r.o.Now != nil {
		return r.o.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *runner) run(parent context.Context) {
	o := &r.o
	switch o.Mode {
	case "", ModeReport:
		o.Mode = ModeReport
	case ModeOff:
		r.res.Mode = ModeOff
		r.skip("disabled (" + EnvVar + "=off)")
		return
	case ModeReap:
		r.warn("reap mode is not available yet (libs#79); running in report mode, which changes nothing")
		o.Mode = ModeReport
	default:
		r.warn("unknown mode %q; running in report mode, which changes nothing", o.Mode)
		o.Mode = ModeReport
	}
	r.res.Mode = o.Mode

	var missing []string
	if o.Tool == "" {
		missing = append(missing, "Tool")
	}
	if o.Account == "" {
		missing = append(missing, "Account")
	}
	if len(o.Regions) == 0 {
		missing = append(missing, "Regions")
	}
	if o.Lifecycle == nil {
		missing = append(missing, "Lifecycle")
	}
	if len(missing) > 0 {
		r.warn("not configured: %s missing", strings.Join(missing, ", "))
		r.skip("not configured: " + strings.Join(missing, ", ") + " missing")
		return
	}
	if parent.Err() != nil {
		r.skip("context already done: " + parent.Err().Error())
		return
	}
	if o.Grace <= 0 {
		o.Grace = DefaultGrace
	}
	if o.Budget <= 0 {
		o.Budget = DefaultBudget
	}
	if o.MinInterval <= 0 {
		o.MinInterval = DefaultMinInterval
	}
	if o.StatePath == "" {
		p, err := DefaultStatePath()
		if err != nil {
			r.warn("%v", err)
			r.skip("no state file: cannot rate-limit")
			return
		}
		o.StatePath = p
	}

	scopes := r.scopes()
	if len(scopes) == 0 {
		r.skip("no usable region")
		return
	}

	st, err := loadState(o.StatePath)
	if err != nil {
		r.warn("%v", err)
	}
	now := r.now()
	var dueScopes []string
	var next time.Time
	for _, sc := range scopes {
		last := st.Runs[stateKey(string(o.Tool), o.Account, sc)].LastRun
		if due(last, now, o.MinInterval) {
			dueScopes = append(dueScopes, sc)
		} else if n := last.Add(o.MinInterval); next.IsZero() || n.Before(next) {
			next = n
		}
	}
	if len(dueScopes) == 0 {
		r.skip(fmt.Sprintf("rate-limited: every scope was checked within %s; next due %s", o.MinInterval, next.Format(time.RFC3339)))
		return
	}

	r.res.Ran = true
	ctx, cancel := context.WithTimeoutCause(parent, o.Budget, errBudget)
	defer cancel()
	var unfinished []string
	for _, sc := range dueScopes {
		key := stateKey(string(o.Tool), o.Account, sc)
		rec := st.Runs[key]
		if ctx.Err() != nil {
			unfinished = append(unfinished, sc)
			continue
		}
		rec.LastAttempt = now
		r.scope(ctx, sc, scopes)
		if err := ctx.Err(); err != nil {
			rec.Incomplete = stopReason(ctx)
			unfinished = append(unfinished, sc)
		} else {
			rec.LastRun, rec.Incomplete = now, ""
		}
		st.Runs[key] = rec
	}
	if len(unfinished) > 0 {
		r.warn("%s before %s were checked to the end; they run again on the next invocation", stopReason(ctx), strings.Join(unfinished, ", "))
	}
	if err := saveState(o.StatePath, st); err != nil {
		r.warn("%v", err)
	}
}

func staleDetail(l *lifecycle.LeaseInfo) string {
	d := fmt.Sprintf("lease held by %s went stale; last heartbeat %s", l.Owner, l.Heartbeat.Format(time.RFC3339))
	if l.Step != "" {
		d += fmt.Sprintf(", at step %q", l.Step)
	}
	return d
}

func stopReason(ctx context.Context) string {
	if errors.Is(context.Cause(ctx), errBudget) {
		return "budget exceeded"
	}
	return "cancelled"
}

// scopes returns the regions, deduplicated, then lifecycle.ScopeGlobal if the
// Client has a control region.
func (r *runner) scopes() []string {
	var out []string
	seen := map[string]bool{}
	for _, reg := range r.o.Regions {
		switch {
		case reg == "" || reg == lifecycle.ScopeGlobal || strings.Contains(reg, "#"):
			r.warn("ignoring region %q", reg)
		case !seen[reg]:
			seen[reg] = true
			out = append(out, reg)
		}
	}
	if _, err := r.o.Lifecycle.RegionFor(lifecycle.ScopeGlobal); err != nil {
		if len(r.o.Residue) > 0 {
			r.warn("residue not judged: the lifecycle Client has no ControlRegion, so the global scope cannot be read")
		} else {
			r.note("global scope skipped: the lifecycle Client has no ControlRegion")
		}
		return out
	}
	return append(out, lifecycle.ScopeGlobal)
}

// scope checks one scope.
func (r *runner) scope(ctx context.Context, sc string, all []string) {
	o := &r.o
	key := lifecycle.ItemKey(o.Tool, sc)
	it, ok, err := call2(ctx, func() (lifecycle.Item, bool, error) { return o.Lifecycle.Get(ctx, o.Tool, sc) })
	switch {
	case err != nil:
		r.warn("read %s: %v", key, err)
	case ok && it.Lease != nil && o.Lifecycle.LeaseStale(it):
		r.would(Action{
			Kind:   KindResumeTeardown,
			Scope:  sc,
			Region: sc,
			Target: key,
			Detail: staleDetail(it.Lease),
		})
		r.plan(ctx, sc, r.scopeResources(ctx, sc))
	case ok && it.Lease != nil:
		r.note("%s: teardown in progress by %s (heartbeat %s); left alone", key, it.Lease.Owner, it.Lease.Heartbeat.Format(time.RFC3339))
	case !o.FullVisibility:
		r.note("%s: idle check skipped: %s has a backstop or cannot read every activity signal (FullVisibility is false)", key, o.Tool)
	default:
		r.idle(ctx, sc, it, ok)
	}

	if ctx.Err() == nil {
		r.table(ctx, sc)
	}
	if sc == lifecycle.ScopeGlobal && ctx.Err() == nil {
		r.residue(ctx, all)
	}
}

// idle is the idle check of a tool with no backstop, in report mode: what
// Observe and Claim would do, worked out from Get and the tool's Activity.
func (r *runner) idle(ctx context.Context, sc string, it lifecycle.Item, exists bool) {
	o := &r.o
	key := lifecycle.ItemKey(o.Tool, sc)
	if o.Activity == nil {
		r.warn("%s: idle check skipped: FullVisibility is set but Activity is nil", key)
		return
	}
	active, err := call1(ctx, func() (bool, error) { return o.Activity(ctx, sc) })
	if err != nil {
		r.warn("%s: activity read failed, counted as active: %v", key, err)
		active = true
	}
	if exists && len(it.Holds) > 0 {
		r.note("%s: held by %v; counted as active", key, it.Holds)
		return
	}
	if active {
		r.note("%s: active; nothing to tear down", key)
		return
	}
	var idleSince time.Time
	if exists && (it.State == lifecycle.StateIdle || it.State == lifecycle.StateTeardown) {
		idleSince = it.IdleSince
	}
	if idleSince.IsZero() {
		if !exists && len(r.scopeResources(ctx, sc)) == 0 {
			r.note("%s: no lifecycle item and nothing in the footprint", key)
			return
		}
		r.note("%s: idle; Observe would start the %s grace now (report mode does not write it)", key, o.Grace)
		return
	}
	idleFor := r.now().Sub(idleSince)
	if idleFor < o.Grace {
		r.note("%s: idle for %s of the %s grace", key, idleFor.Round(time.Minute), o.Grace)
		return
	}
	r.would(Action{
		Kind:   KindIdleTeardown,
		Scope:  sc,
		Region: sc,
		Target: key,
		Detail: fmt.Sprintf("idle since %s, %s; grace %s", idleSince.Format(time.RFC3339), idleFor.Round(time.Minute), o.Grace),
	})
	r.plan(ctx, sc, r.scopeResources(ctx, sc))
}

// table reports an empty lifecycle table in the scope's region, once per
// region.
func (r *runner) table(ctx context.Context, sc string) {
	region, err := r.o.Lifecycle.RegionFor(sc)
	if err != nil || r.tablesChecked[region] {
		return
	}
	if r.tablesChecked == nil {
		r.tablesChecked = map[string]bool{}
	}
	r.tablesChecked[region] = true
	items, exists, err := call2(ctx, func() ([]lifecycle.Item, bool, error) { return r.o.Lifecycle.ListItems(ctx, region) })
	switch {
	case err != nil:
		r.warn("list %s in %s: %v", lifecycle.TableName, region, err)
	case !exists:
	case len(items) == 0:
		r.would(Action{Kind: KindCloseTable, Scope: sc, Region: region, Target: lifecycle.TableName,
			Detail: "no tool item left; CloseTableIfEmpty re-checks under its sentinel"})
	default:
		r.note("%s in %s holds %d tool item(s); kept", lifecycle.TableName, region, len(items))
	}
}

// residue reports the tool's residue as removable once no lifecycle item of
// the tool remains in any scope.
func (r *runner) residue(ctx context.Context, all []string) {
	o := &r.o
	if len(o.Residue) == 0 {
		return
	}
	for _, sc := range all {
		key := lifecycle.ItemKey(o.Tool, sc)
		it, ok, err := call2(ctx, func() (lifecycle.Item, bool, error) { return o.Lifecycle.Get(ctx, o.Tool, sc) })
		if err != nil {
			r.warn("residue not judged: read %s: %v", key, err)
			return
		}
		if ok {
			r.note("residue kept: %s remains (state %s)", key, it.State)
			return
		}
	}
	r.plan(ctx, lifecycle.ScopeGlobal, o.Residue)
}

// scopeResources returns the footprint's resources in scope, loading the
// footprint the first time.
func (r *runner) scopeResources(ctx context.Context, sc string) []footprint.Resource {
	if !r.footprintDone {
		r.footprintDone = true
		r.footprint = r.loadFootprint(ctx)
	}
	if r.footprint == nil {
		return nil
	}
	// A footprint resource's Region is its region, or "global" for IAM and
	// other account-wide resources: the same values as the scopes.
	var out []footprint.Resource
	for _, res := range r.footprint.Resources {
		if res.Region == sc {
			out = append(out, res)
		}
	}
	return out
}

// loadFootprint calls the tool's Footprint, and returns its report if it is
// for this tool and account.
func (r *runner) loadFootprint(ctx context.Context) *footprint.Report {
	o := &r.o
	if o.Footprint == nil {
		r.note("no Footprint: plans list no resources")
		return nil
	}
	rep, err := call1(ctx, func() (*footprint.Report, error) { return o.Footprint(ctx) })
	switch {
	case err != nil:
		r.warn("footprint: %v", err)
		return nil
	case rep == nil:
		r.warn("footprint: no report")
		return nil
	case rep.Tool != o.Tool || rep.Account != o.Account:
		r.warn("footprint: report is for %s in %q, not %s in %q; ignored", rep.Tool, rep.Account, o.Tool, o.Account)
		return nil
	}
	for _, w := range rep.Warnings {
		r.warn("footprint: %s", w)
	}
	return rep
}

// plan works out, with footprint.Execute in report mode, which of resources
// a teardown in sc would delete.
func (r *runner) plan(ctx context.Context, sc string, resources []footprint.Resource) {
	o := &r.o
	if len(resources) == 0 {
		r.note("%s: no resources to plan", sc)
		return
	}
	p, err := footprint.NewPlan(o.Tool, o.Patterns, resources)
	if err != nil {
		r.warn("%s: plan: %v", sc, err)
		return
	}
	d, detail := o.Deleter, ""
	if d == nil {
		d, detail = asPlanned{}, "not re-checked live: no Deleter"
	}
	// ModeReport, always, with no lease: Execute then calls only Describe.
	out, err := call1(ctx, func() (*footprint.Result, error) {
		return footprint.Execute(ctx, p, footprint.ModeReport, d, nil)
	})
	if out == nil {
		r.warn("%s: plan: %v", sc, err)
		return
	}
	for i, a := range out.Actions {
		res := p.Steps[i].Resource
		switch a.Outcome {
		case footprint.OutcomeWouldDelete:
			r.would(Action{Kind: KindDelete, Scope: sc, Region: res.Region, Target: a.ARN, Type: a.Type, Name: a.Name, Detail: detail})
		case footprint.OutcomeFailed:
			r.warn("%s: %s: %s: %v", sc, a.ARN, a.Reason, a.Err)
		default:
			r.note("%s: %s: %s (%s)", sc, a.ARN, a.Outcome, a.Reason)
		}
	}
}

// asPlanned is the Deleter used when the tool gives none: Describe reports
// the resource as planned, and Delete refuses. Execute in report mode never
// calls Delete.
type asPlanned struct{}

func (asPlanned) Describe(_ context.Context, r footprint.Resource) (footprint.Resource, error) {
	return r, nil
}

func (asPlanned) Delete(context.Context, footprint.Resource) error {
	return errors.New("lastmile: no Deleter")
}

// call1 runs fn in a goroutine and returns its result, or ctx's error as soon
// as ctx is done, so that a callback that ignores its context cannot hold Run
// past its budget. A panic in fn becomes an error.
func call1[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	v, _, err := call2(ctx, func() (T, struct{}, error) {
		v, err := fn()
		return v, struct{}{}, err
	})
	return v, err
}

func call2[A, B any](ctx context.Context, fn func() (A, B, error)) (A, B, error) {
	type result struct {
		a   A
		b   B
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var res result
		defer func() {
			if p := recover(); p != nil {
				res = result{err: fmt.Errorf("panic: %v", p)}
			}
			ch <- res
		}()
		res.a, res.b, res.err = fn()
	}()
	select {
	case res := <-ch:
		return res.a, res.b, res.err
	case <-ctx.Done():
		var a A
		var b B
		return a, b, context.Cause(ctx)
	}
}
