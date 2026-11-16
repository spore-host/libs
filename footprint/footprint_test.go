package footprint_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/libs/footprint"
	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/lifecycle/lifecycletest"
	"github.com/spore-host/libs/tags"
)

const tool = tags.ToolLagotto

var (
	ctx      = context.Background()
	now      = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	patterns = []footprint.NamePattern{
		footprint.MustPattern("", `lagotto-[a-z0-9-]+`),
		footprint.MustPattern("AWS::IAM::Role", `LagottoPoller`),
	}
)

func managed() map[string]string { return tags.For(tool, "us-west-2", now, "") }

// res is a resource the guard accepts.
func res(arn, name string, deps ...string) footprint.Resource {
	return footprint.Resource{
		ARN: arn, Type: "AWS::SQS::Queue", Region: "us-west-2", Name: name,
		Ownership: footprint.OwnershipTagged, Class: footprint.ClassResidue,
		DependsOn: deps, Tags: managed(),
	}
}

func TestDeletableTruthTable(t *testing.T) {
	base := res("arn:aws:sqs:us-west-2:1:lagotto-q", "lagotto-q")
	if ok, why := footprint.Deletable(base, tool, patterns); !ok {
		t.Fatalf("base resource refused: %s", why)
	}
	for _, c := range []footprint.Class{footprint.ClassLitter, footprint.ClassResidue, footprint.ClassActive} {
		r := base
		r.Class = c
		if ok, why := footprint.Deletable(r, tool, patterns); !ok {
			t.Errorf("class %s refused: %s", c, why)
		}
	}
	role := base
	role.Type, role.Name = "AWS::IAM::Role", "LagottoPoller"
	if ok, why := footprint.Deletable(role, tool, patterns); !ok {
		t.Errorf("typed pattern refused: %s", why)
	}

	with := func(f func(r *footprint.Resource)) footprint.Resource {
		r := base
		r.Tags = managed()
		f(&r)
		return r
	}
	for _, c := range []struct {
		name string
		r    footprint.Resource
		tool tags.Tool
		want string
	}{
		{"legacy-name ownership", with(func(r *footprint.Resource) { r.Ownership = footprint.OwnershipLegacyName }), tool, "legacy-name"},
		{"foreign ownership", with(func(r *footprint.Resource) { r.Ownership = footprint.OwnershipForeign }), tool, "foreign"},
		{"no ownership", with(func(r *footprint.Resource) { r.Ownership = "" }), tool, "ownership"},
		{"untagged", with(func(r *footprint.Resource) { r.Tags = nil }), tool, "not tagged"},
		{"another tool's", with(func(r *footprint.Resource) { r.Tags[tags.ManagedBy] = string(tags.ToolSpawn) }), tool, "not tagged"},
		{"wrong tool asking", base, tags.ToolSpawn, "not tagged"},
		{"no tool asking", base, "", "no tool"},
		{"service", with(func(r *footprint.Resource) { r.Tags[tags.Role] = tags.RoleService }), tool, "service"},
		{"permanent tag", with(func(r *footprint.Resource) { r.Tags[tags.Permanent] = tags.PermanentTrue }), tool, "permanent"},
		{"boundary by ARN", with(func(r *footprint.Resource) {
			r.ARN = "arn:aws:iam::1:policy/spore-host/spawn-instance-boundary"
		}), tool, "spawn-instance-boundary"},
		{"boundary by name", with(func(r *footprint.Resource) {
			r.Type, r.Name = "AWS::IAM::ManagedPolicy", footprint.BoundaryPolicyName
		}), tool, "spawn-instance-boundary"},
		{"user data", with(func(r *footprint.Resource) { r.Class = footprint.ClassUserData }), tool, "user-data"},
		{"permanent class", with(func(r *footprint.Resource) { r.Class = footprint.ClassPermanent }), tool, "permanent"},
		{"no class", with(func(r *footprint.Resource) { r.Class = "" }), tool, "not a known class"},
		{"unknown class", with(func(r *footprint.Resource) { r.Class = "orphan" }), tool, "not a known class"},
		{"name matches no pattern", with(func(r *footprint.Resource) { r.Name = "spawn-q" }), tool, "matches none"},
		{"pattern is anchored", with(func(r *footprint.Resource) { r.Name = "x-lagotto-q" }), tool, "matches none"},
		{"typed pattern, other type", with(func(r *footprint.Resource) { r.Name = "LagottoPoller" }), tool, "matches none"},
	} {
		ok, why := footprint.Deletable(c.r, c.tool, patterns)
		if ok || !strings.Contains(why, c.want) {
			t.Errorf("%s: Deletable = %v, %q; want false with %q", c.name, ok, why, c.want)
		}
	}
	// Without patterns nothing matches; a pattern with no regexp matches
	// nothing.
	if ok, _ := footprint.Deletable(base, tool, nil); ok {
		t.Error("deletable with no patterns")
	}
	if ok, _ := footprint.Deletable(base, tool, []footprint.NamePattern{{}}); ok {
		t.Error("deletable by a zero pattern")
	}
	// A legacy-name resource with every other condition met is still refused.
	legacy := base
	legacy.Ownership = footprint.OwnershipLegacyName
	if ok, _ := footprint.Deletable(legacy, tool, patterns); ok {
		t.Error("legacy-name resource deletable")
	}
}

func TestNewPattern(t *testing.T) {
	if _, err := footprint.NewPattern("", "("); err == nil {
		t.Error("bad expression compiled")
	}
	defer func() {
		if recover() == nil {
			t.Error("MustPattern did not panic on a bad expression")
		}
	}()
	footprint.MustPattern("", "(")
}

func arns(p *footprint.Plan) []string {
	var out []string
	for _, s := range p.Steps {
		out = append(out, s.Resource.ARN)
	}
	return out
}

func TestPlanOrdersDependentsFirst(t *testing.T) {
	// instance depends on sg and role; sg depends on vpc; role is
	// independent of vpc. "ext" is not in the plan.
	in := []footprint.Resource{
		res("vpc", "lagotto-vpc"),
		res("sg", "lagotto-sg", "vpc", "vpc", "ext"),
		res("role", "lagotto-role"),
		res("instance", "lagotto-i", "sg", "role"),
		res("lone", "lagotto-lone"),
	}
	p, err := footprint.NewPlan(tool, patterns, in)
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, a := range arns(p) {
		pos[a] = i
	}
	for _, e := range [][2]string{{"instance", "sg"}, {"instance", "role"}, {"sg", "vpc"}} {
		if pos[e[0]] > pos[e[1]] {
			t.Errorf("%s is deleted after %s, which it depends on: %v", e[0], e[1], arns(p))
		}
	}
	// Ties keep the input order.
	if want := []string{"instance", "sg", "vpc", "role", "lone"}; !reflect.DeepEqual(arns(p), want) {
		t.Errorf("order = %v, want %v", arns(p), want)
	}
	if got := p.Steps[pos["instance"]].Then; !reflect.DeepEqual(got, []int{pos["sg"], pos["role"]}) {
		t.Errorf("instance's Then = %v", got)
	}
	if got := p.Steps[pos["sg"]].Then; !reflect.DeepEqual(got, []int{pos["vpc"]}) {
		t.Errorf("sg's Then = %v (the duplicate and the external dependency must be dropped)", got)
	}
	if p.Tool != tool || len(p.Patterns) != len(patterns) {
		t.Error("plan lost its tool or patterns")
	}
	if empty, err := footprint.NewPlan(tool, patterns, nil); err != nil || len(empty.Steps) != 0 {
		t.Errorf("empty plan = %v, %v", empty, err)
	}
}

func TestPlanDetectsCycles(t *testing.T) {
	_, err := footprint.NewPlan(tool, patterns, []footprint.Resource{
		res("a", "lagotto-a", "b"),
		res("b", "lagotto-b", "c"),
		res("c", "lagotto-c", "a", "d"),
		res("d", "lagotto-d"),
		res("e", "lagotto-e", "a"), // depends on the cycle: orderable
	})
	var ce *footprint.CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("cycle = %v", err)
	}
	if want := []string{"a", "b", "c", "d"}; !reflect.DeepEqual(ce.ARNs, want) {
		t.Errorf("cycle ARNs = %v, want %v", ce.ARNs, want)
	}
	if !strings.Contains(err.Error(), "a, b, c, d") {
		t.Errorf("error = %q", err)
	}
	if _, err := footprint.NewPlan(tool, patterns, []footprint.Resource{res("a", "lagotto-a", "a")}); !errors.As(err, &ce) {
		t.Errorf("self-dependency = %v", err)
	}
	if _, err := footprint.NewPlan(tool, patterns, []footprint.Resource{res("a", "x"), res("a", "y")}); err == nil {
		t.Error("duplicate ARN accepted")
	}
	if _, err := footprint.NewPlan(tool, patterns, []footprint.Resource{res("", "x")}); err == nil {
		t.Error("empty ARN accepted")
	}
}

// fakeDeleter holds the live state of the resources.
type fakeDeleter struct {
	live        map[string]footprint.Resource
	describeErr map[string]error
	deleteErr   map[string]error
	calls       []string
	onDelete    func(arn string)
}

func newDeleter(rs ...footprint.Resource) *fakeDeleter {
	d := &fakeDeleter{live: map[string]footprint.Resource{}, describeErr: map[string]error{}, deleteErr: map[string]error{}}
	for _, r := range rs {
		d.live[r.ARN] = r
	}
	return d
}

func (d *fakeDeleter) Describe(_ context.Context, r footprint.Resource) (footprint.Resource, error) {
	d.calls = append(d.calls, "describe "+r.ARN)
	if err := d.describeErr[r.ARN]; err != nil {
		return footprint.Resource{}, err
	}
	l, ok := d.live[r.ARN]
	if !ok {
		return footprint.Resource{}, footprint.ErrNotFound
	}
	// Report only what a describe call sees: tags (copied).
	cp := map[string]string{}
	for k, v := range l.Tags {
		cp[k] = v
	}
	return footprint.Resource{ARN: l.ARN, Tags: cp, Class: l.Class}, nil
}

func (d *fakeDeleter) Delete(_ context.Context, r footprint.Resource) error {
	d.calls = append(d.calls, "delete "+r.ARN)
	if d.onDelete != nil {
		d.onDelete(r.ARN)
	}
	if err := d.deleteErr[r.ARN]; err != nil {
		return err
	}
	if _, ok := d.live[r.ARN]; !ok {
		return footprint.ErrNotFound
	}
	delete(d.live, r.ARN)
	return nil
}

func (d *fakeDeleter) deletes() []string {
	var out []string
	for _, c := range d.calls {
		if strings.HasPrefix(c, "delete ") {
			out = append(out, strings.TrimPrefix(c, "delete "))
		}
	}
	return out
}

// claim returns a real teardown lease on a lifecycle fake, and a second
// client on the same table.
func claim(t *testing.T) (*lifecycle.Lease, *lifecycle.Client, *lifecycletest.Fake) {
	t.Helper()
	f := lifecycletest.New()
	mk := func(owner string) *lifecycle.Client {
		c, err := lifecycle.New(lifecycle.Options{
			DynamoDB: func(string) (lifecycle.API, error) { return f, nil },
			Owner:    owner,
			Now:      func() time.Time { return now },
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := mk("backstop")
	if _, _, err := c.Observe(ctx, tool, "us-west-2", false); err != nil {
		t.Fatal(err)
	}
	l, err := c.Claim(ctx, tool, "us-west-2", 0)
	if err != nil {
		t.Fatal(err)
	}
	return l, mk("other"), f
}

func plan(t *testing.T, rs ...footprint.Resource) *footprint.Plan {
	t.Helper()
	p, err := footprint.NewPlan(tool, patterns, rs)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func outcomes(r *footprint.Result) map[string]footprint.Outcome {
	m := map[string]footprint.Outcome{}
	for _, a := range r.Actions {
		m[a.ARN] = a.Outcome
	}
	return m
}

func TestExecuteReapDeletesInOrder(t *testing.T) {
	rs := []footprint.Resource{res("vpc", "lagotto-vpc"), res("sg", "lagotto-sg", "vpc"), res("i", "lagotto-i", "sg")}
	d := newDeleter(rs...)
	l, c, _ := claim(t)
	r, err := footprint.Execute(ctx, plan(t, rs...), footprint.ModeReap, d, l)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.deletes(); !reflect.DeepEqual(got, []string{"i", "sg", "vpc"}) {
		t.Errorf("deleted %v, want dependents first", got)
	}
	// Every delete was preceded by its re-check.
	want := []string{"describe i", "delete i", "describe sg", "delete sg", "describe vpc", "delete vpc"}
	if !reflect.DeepEqual(d.calls, want) {
		t.Errorf("calls = %v", d.calls)
	}
	if r.Count(footprint.OutcomeDeleted) != 3 || r.Mode != footprint.ModeReap || r.Tool != string(tool) {
		t.Errorf("result = %+v", r)
	}
	// The lease heartbeat named each step.
	it, _, _ := c.Get(ctx, tool, "us-west-2")
	if it.Lease == nil || it.Lease.Step != "delete AWS::SQS::Queue vpc" {
		t.Errorf("lease after the run = %+v", it.Lease)
	}
}

func TestExecuteReportModeDeletesNothing(t *testing.T) {
	rs := []footprint.Resource{res("a", "lagotto-a"), res("b", "lagotto-b", "a"), res("user", "lagotto-results")}
	rs[2].Class = footprint.ClassUserData
	for _, withLease := range []bool{false, true} {
		d := newDeleter(rs...)
		var l *lifecycle.Lease
		var c *lifecycle.Client
		if withLease {
			l, c, _ = claim(t)
		}
		r, err := footprint.Execute(ctx, plan(t, rs...), footprint.ModeReport, d, l)
		if err != nil {
			t.Fatal(err)
		}
		if len(d.deletes()) != 0 {
			t.Errorf("report mode deleted %v", d.deletes())
		}
		want := map[string]footprint.Outcome{"b": footprint.OutcomeWouldDelete, "a": footprint.OutcomeWouldDelete, "user": footprint.OutcomeKept}
		if got := outcomes(r); !reflect.DeepEqual(got, want) {
			t.Errorf("report outcomes = %v, want %v", got, want)
		}
		if withLease {
			if it, _, _ := c.Get(ctx, tool, "us-west-2"); it.Lease.Step == "" {
				t.Error("report mode with a lease did not heartbeat")
			}
		}
	}
}

func TestExecuteRefusesWhatChangedSinceThePlan(t *testing.T) {
	for name, change := range map[string]func(r *footprint.Resource){
		"tag removed":      func(r *footprint.Resource) { delete(r.Tags, tags.ManagedBy) },
		"retagged":         func(r *footprint.Resource) { r.Tags[tags.ManagedBy] = string(tags.ToolSpawn) },
		"made permanent":   func(r *footprint.Resource) { r.Tags[tags.Permanent] = tags.PermanentTrue },
		"became service":   func(r *footprint.Resource) { r.Tags[tags.Role] = tags.RoleService },
		"became user data": func(r *footprint.Resource) { r.Class = footprint.ClassUserData },
	} {
		t.Run(name, func(t *testing.T) {
			rs := []footprint.Resource{res("dep", "lagotto-dep"), res("x", "lagotto-x", "dep"), res("other", "lagotto-other")}
			p := plan(t, rs...)
			d := newDeleter(rs...)
			live := d.live["x"]
			live.Tags = managed()
			change(&live)
			d.live["x"] = live
			l, _, _ := claim(t)
			r, err := footprint.Execute(ctx, p, footprint.ModeReap, d, l)
			if err != nil {
				t.Fatalf("a refusal is not an error: %v", err)
			}
			want := map[string]footprint.Outcome{"x": footprint.OutcomeRefused, "dep": footprint.OutcomeSkipped, "other": footprint.OutcomeDeleted}
			if got := outcomes(r); !reflect.DeepEqual(got, want) {
				t.Errorf("outcomes = %v, want %v", got, want)
			}
			if got := d.deletes(); !reflect.DeepEqual(got, []string{"other"}) {
				t.Errorf("deleted %v", got)
			}
		})
	}
}

func TestExecuteNotFoundIsSuccess(t *testing.T) {
	rs := []footprint.Resource{res("dep", "lagotto-dep"), res("gone-early", "lagotto-g1", "dep"), res("gone-late", "lagotto-g2", "dep")}
	d := newDeleter(rs...)
	delete(d.live, "gone-early")
	d.deleteErr["gone-late"] = errors.Join(errors.New("NoSuchEntity"), footprint.ErrNotFound)
	l, _, _ := claim(t)
	r, err := footprint.Execute(ctx, plan(t, rs...), footprint.ModeReap, d, l)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]footprint.Outcome{"gone-early": footprint.OutcomeGone, "gone-late": footprint.OutcomeGone, "dep": footprint.OutcomeDeleted}
	if got := outcomes(r); !reflect.DeepEqual(got, want) {
		t.Errorf("outcomes = %v, want %v", got, want)
	}
	if !contains(d.calls, "delete dep") || contains(d.calls, "delete gone-early") {
		t.Errorf("calls = %v", d.calls)
	}
}

func TestExecuteStopsWhenTheLeaseIsLost(t *testing.T) {
	rs := []footprint.Resource{res("a", "lagotto-a"), res("b", "lagotto-b"), res("c", "lagotto-c"), res("d", "lagotto-d")}
	d := newDeleter(rs...)
	l, other, _ := claim(t)
	d.onDelete = func(arn string) {
		if arn == "b" {
			// A user starts using the tool mid-teardown: activity observed.
			if _, _, err := other.Observe(ctx, tool, "us-west-2", true); err != nil {
				t.Error(err)
			}
		}
	}
	r, err := footprint.Execute(ctx, plan(t, rs...), footprint.ModeReap, d, l)
	if !errors.Is(err, lifecycle.ErrLeaseLost) {
		t.Fatalf("Execute = %v, want ErrLeaseLost", err)
	}
	want := map[string]footprint.Outcome{"a": footprint.OutcomeDeleted, "b": footprint.OutcomeDeleted, "c": footprint.OutcomeStopped, "d": footprint.OutcomeNotRun}
	if got := outcomes(r); !reflect.DeepEqual(got, want) {
		t.Errorf("outcomes = %v, want %v", got, want)
	}
	if got := d.deletes(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("deleted %v after losing the lease", got)
	}
	if r.Actions[2].Err == nil {
		t.Error("stopped step has no error")
	}
}

func TestExecuteFailureSkipsDependencies(t *testing.T) {
	// i depends on sg, sg on vpc; the i delete fails, so sg and vpc (which
	// it still uses) are skipped. Unrelated work carries on. A describe
	// failure skips the same way.
	rs := []footprint.Resource{
		res("vpc", "lagotto-vpc"), res("sg", "lagotto-sg", "vpc"), res("i", "lagotto-i", "sg"),
		res("q", "lagotto-q"),
		res("t", "lagotto-t"), res("u", "lagotto-u", "t"),
	}
	d := newDeleter(rs...)
	boom, bang := errors.New("DependencyViolation"), errors.New("throttled")
	d.deleteErr["i"] = boom
	d.describeErr["u"] = bang
	l, _, _ := claim(t)
	r, err := footprint.Execute(ctx, plan(t, rs...), footprint.ModeReap, d, l)
	if !errors.Is(err, boom) || !errors.Is(err, bang) {
		t.Fatalf("Execute = %v, want both failures", err)
	}
	want := map[string]footprint.Outcome{
		"i": footprint.OutcomeFailed, "sg": footprint.OutcomeSkipped, "vpc": footprint.OutcomeSkipped,
		"q": footprint.OutcomeDeleted, "u": footprint.OutcomeFailed, "t": footprint.OutcomeSkipped,
	}
	if got := outcomes(r); !reflect.DeepEqual(got, want) {
		t.Errorf("outcomes = %v, want %v", got, want)
	}
	for _, a := range r.Actions {
		if a.ARN == "vpc" && !strings.Contains(a.Reason, "sg depends on it and was skipped") {
			t.Errorf("vpc skip reason = %q", a.Reason)
		}
	}
	if got := d.deletes(); !reflect.DeepEqual(got, []string{"i", "q"}) {
		t.Errorf("deleted %v", got)
	}
}

func TestExecuteKeptResourceProtectsItsDependencies(t *testing.T) {
	// A user-data bucket depends on a KMS key the tool made: the key must
	// survive while the bucket does.
	bucket := res("bucket", "lagotto-results", "key")
	bucket.Class = footprint.ClassUserData
	rs := []footprint.Resource{res("key", "lagotto-key"), bucket}
	d := newDeleter(rs...)
	l, _, _ := claim(t)
	r, err := footprint.Execute(ctx, plan(t, rs...), footprint.ModeReap, d, l)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]footprint.Outcome{"bucket": footprint.OutcomeKept, "key": footprint.OutcomeSkipped}
	if got := outcomes(r); !reflect.DeepEqual(got, want) {
		t.Errorf("outcomes = %v, want %v", got, want)
	}
	if len(d.calls) != 0 {
		t.Errorf("calls = %v", d.calls)
	}
}

func TestExecuteArguments(t *testing.T) {
	p := plan(t, res("a", "lagotto-a"))
	d := newDeleter()
	l, _, _ := claim(t)
	for name, err := range map[string]error{
		"nil plan":           second(footprint.Execute(ctx, nil, footprint.ModeReport, d, nil)),
		"bad mode":           second(footprint.Execute(ctx, p, "delete", d, l)),
		"reap without lease": second(footprint.Execute(ctx, p, footprint.ModeReap, d, nil)),
		"nil deleter":        second(footprint.Execute(ctx, p, footprint.ModeReport, nil, nil)),
	} {
		if err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func second(_ *footprint.Result, err error) error { return err }

func TestExecuteCancelledContext(t *testing.T) {
	rs := []footprint.Resource{res("a", "lagotto-a"), res("b", "lagotto-b")}
	c, cancel := context.WithCancel(ctx)
	d := newDeleter(rs...)
	d.onDelete = func(string) { cancel() }
	l, _, _ := claim(t)
	r, err := footprint.Execute(c, plan(t, rs...), footprint.ModeReap, d, l)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute = %v", err)
	}
	if got := outcomes(r); got["a"] != footprint.OutcomeDeleted || got["b"] != footprint.OutcomeNotRun {
		t.Errorf("outcomes = %v", got)
	}
}

func TestByType(t *testing.T) {
	q := newDeleter(res("q", "lagotto-q"))
	bt := footprint.ByType{"AWS::SQS::Queue": q}
	if _, err := bt.Describe(ctx, res("q", "lagotto-q")); err != nil {
		t.Error(err)
	}
	if err := bt.Delete(ctx, res("q", "lagotto-q")); err != nil {
		t.Error(err)
	}
	role := res("r", "LagottoPoller")
	role.Type = "AWS::IAM::Role"
	if _, err := bt.Describe(ctx, role); err == nil || !strings.Contains(err.Error(), "no deleter") {
		t.Errorf("Describe of an unregistered type = %v", err)
	}
	if err := bt.Delete(ctx, role); err == nil {
		t.Error("Delete of an unregistered type succeeded")
	}
	// Through Execute, an unregistered type is a failed step, not a crash.
	p := plan(t, role)
	l, _, _ := claim(t)
	r, err := footprint.Execute(ctx, p, footprint.ModeReap, bt, l)
	if err == nil || r.Count(footprint.OutcomeFailed) != 1 {
		t.Errorf("Execute with an unregistered type = %v, %+v", err, r)
	}
}

func TestReportJSON(t *testing.T) {
	rep := footprint.NewReport(tool, "123456789012", now.In(time.FixedZone("x", 3600)))
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema":1,"tool":"lagotto","account":"123456789012","generated_at":"2026-11-16T12:00:00Z","resources":[],"warnings":[]}`
	if string(b) != want {
		t.Errorf("empty report JSON\n got %s\nwant %s", b, want)
	}
	r := footprint.Resource{ARN: "arn:x", Type: "AWS::SQS::Queue", Region: "us-west-2", Name: "lagotto-q",
		Ownership: footprint.OwnershipTagged, Class: footprint.ClassResidue}
	b, _ = json.Marshal(r)
	want = `{"arn":"arn:x","type":"AWS::SQS::Queue","region":"us-west-2","scope":"","name":"lagotto-q","ownership":"tagged","class":"residue","deletable":false,"reason":"","depends_on":[],"tags":{}}`
	if string(b) != want {
		t.Errorf("resource JSON\n got %s\nwant %s", b, want)
	}

	r.DependsOn, r.Tags, r.Deletable = []string{"arn:y"}, map[string]string{"k": "v"}, true
	rep.Resources, rep.Warnings = []footprint.Resource{r}, []string{"listing S3 failed"}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(rep); err != nil {
		t.Fatal(err)
	}
	back, err := footprint.Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, rep) {
		t.Errorf("round trip\n got %+v\nwant %+v", back, rep)
	}
}

func TestDecodeRejects(t *testing.T) {
	for name, in := range map[string]string{
		"not json":        `{`,
		"future schema":   `{"schema":2,"resources":[]}`,
		"no schema":       `{"resources":[]}`,
		"unknown class":   `{"schema":1,"resources":[{"arn":"a","ownership":"tagged","class":"orphan"}]}`,
		"unknown owner":   `{"schema":1,"resources":[{"arn":"a","ownership":"mine","class":"litter"}]}`,
		"bad resource":    `{"schema":1,"resources":[{"arn":7}]}`,
		"bad report type": `[]`,
	} {
		if _, err := footprint.Decode(strings.NewReader(in)); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
	if _, err := footprint.Decode(strings.NewReader(`{"schema":1,"resources":[{"arn":"a","ownership":"legacy-name","class":"user-data"}]}`)); err != nil {
		t.Errorf("valid report rejected: %v", err)
	}
}

func TestValidValues(t *testing.T) {
	for _, o := range []footprint.Ownership{footprint.OwnershipTagged, footprint.OwnershipLegacyName, footprint.OwnershipForeign} {
		if !o.Valid() {
			t.Errorf("%s invalid", o)
		}
	}
	for _, c := range []footprint.Class{footprint.ClassLitter, footprint.ClassUserData, footprint.ClassPermanent, footprint.ClassResidue, footprint.ClassActive} {
		if !c.Valid() {
			t.Errorf("%s invalid", c)
		}
	}
	if footprint.Ownership("").Valid() || footprint.Class("").Valid() {
		t.Error("empty values valid")
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
