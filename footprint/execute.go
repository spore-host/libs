package footprint

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/tags"
)

// Plan is an ordered teardown. Every resource appears once, after every
// resource that depends on it, so dependents are deleted first. It also
// carries the tool and patterns Execute re-applies Deletable with.
type Plan struct {
	Tool     tags.Tool
	Patterns []NamePattern
	Steps    []Step
}

// Step is one resource in a Plan.
type Step struct {
	Resource Resource
	// Then lists the indexes of the later steps that wait for this one:
	// the resources this one depends on (within the plan). If this step does
	// not end with the resource gone, they are skipped.
	Then []int
}

// CycleError is returned by NewPlan when DependsOn has a cycle.
type CycleError struct {
	// ARNs are the resources that could not be ordered, sorted: those on a
	// cycle and those a cycle depends on.
	ARNs []string
}

func (e *CycleError) Error() string {
	return "footprint: dependency cycle among " + strings.Join(e.ARNs, ", ")
}

// NewPlan orders resources for deletion by a topological sort on DependsOn:
// a resource comes before every resource it depends on. Ties keep the input
// order. A DependsOn entry naming an ARN that is not in resources is ignored
// (the dependency is not being deleted). An empty or duplicate ARN is an
// error, and so is a cycle (a *CycleError).
//
// The plan includes every resource, deletable or not: Execute keeps the
// ones Deletable refuses, and skips whatever they depend on.
func NewPlan(tool tags.Tool, patterns []NamePattern, resources []Resource) (*Plan, error) {
	index := make(map[string]int, len(resources))
	for i, r := range resources {
		if r.ARN == "" {
			return nil, fmt.Errorf("footprint: resource %d (%s %q) has no ARN", i, r.Type, r.Name)
		}
		if _, dup := index[r.ARN]; dup {
			return nil, fmt.Errorf("footprint: resource %s listed twice", r.ARN)
		}
		index[r.ARN] = i
	}
	// edges[i] = the resources i depends on; indegree[j] = how many depend on j.
	edges := make([][]int, len(resources))
	indegree := make([]int, len(resources))
	for i, r := range resources {
		seen := map[int]bool{}
		for _, dep := range r.DependsOn {
			j, ok := index[dep]
			if !ok || seen[j] {
				continue
			}
			seen[j] = true
			edges[i] = append(edges[i], j)
			indegree[j]++
		}
	}
	// Kahn's algorithm, always taking the earliest ready resource.
	var ready []int
	for i := range resources {
		if indegree[i] == 0 {
			ready = append(ready, i)
		}
	}
	order := make([]int, 0, len(resources))
	for len(ready) > 0 {
		sort.Ints(ready)
		i := ready[0]
		ready = ready[1:]
		order = append(order, i)
		for _, j := range edges[i] {
			if indegree[j]--; indegree[j] == 0 {
				ready = append(ready, j)
			}
		}
	}
	if len(order) != len(resources) {
		// What Kahn's algorithm could not place is on a cycle, or is
		// (transitively) depended on by something on one.
		placed := make([]bool, len(resources))
		for _, i := range order {
			placed[i] = true
		}
		var stuck []string
		for i, ok := range placed {
			if !ok {
				stuck = append(stuck, resources[i].ARN)
			}
		}
		sort.Strings(stuck)
		return nil, &CycleError{ARNs: stuck}
	}
	pos := make([]int, len(resources))
	for p, i := range order {
		pos[i] = p
	}
	plan := &Plan{Tool: tool, Patterns: patterns, Steps: make([]Step, len(order))}
	for p, i := range order {
		st := Step{Resource: resources[i]}
		for _, j := range edges[i] {
			st.Then = append(st.Then, pos[j])
		}
		sort.Ints(st.Then)
		plan.Steps[p] = st
	}
	return plan, nil
}

// Mode is how Execute runs a plan.
type Mode string

// Modes.
const (
	// ModeReport re-checks every resource and records what would be deleted,
	// deleting nothing.
	ModeReport Mode = "report"
	// ModeReap deletes.
	ModeReap Mode = "reap"
)

// ErrNotFound is what a Deleter returns (or wraps) when the resource does not
// exist. Execute counts it as success: the resource is gone.
var ErrNotFound = errors.New("footprint: resource not found")

// Deleter reads and deletes resources. Each tool implements it for the
// resource types it owns; ByType combines per-type implementations.
type Deleter interface {
	// Describe returns the resource as it is now: at least its live tags,
	// and its name, class and ownership if it can tell. It returns an error
	// wrapping ErrNotFound if the resource is gone.
	Describe(ctx context.Context, r Resource) (Resource, error)
	// Delete deletes the resource. An error wrapping ErrNotFound counts as
	// success.
	Delete(ctx context.Context, r Resource) error
}

// ByType is a Deleter that dispatches on Resource.Type.
type ByType map[string]Deleter

func (b ByType) get(r Resource) (Deleter, error) {
	d, ok := b[r.Type]
	if !ok || d == nil {
		return nil, fmt.Errorf("footprint: no deleter for resource type %q", r.Type)
	}
	return d, nil
}

// Describe calls the Deleter registered for r.Type.
func (b ByType) Describe(ctx context.Context, r Resource) (Resource, error) {
	d, err := b.get(r)
	if err != nil {
		return Resource{}, err
	}
	return d.Describe(ctx, r)
}

// Delete calls the Deleter registered for r.Type.
func (b ByType) Delete(ctx context.Context, r Resource) error {
	d, err := b.get(r)
	if err != nil {
		return err
	}
	return d.Delete(ctx, r)
}

// Outcome is what happened to one step.
type Outcome string

// Outcomes.
const (
	// OutcomeDeleted: deleted (Reap mode).
	OutcomeDeleted Outcome = "deleted"
	// OutcomeWouldDelete: passed every check; Report mode deleted nothing.
	OutcomeWouldDelete Outcome = "would-delete"
	// OutcomeGone: already absent, at the re-check or at the delete.
	OutcomeGone Outcome = "gone"
	// OutcomeKept: Deletable refused it as planned.
	OutcomeKept Outcome = "kept"
	// OutcomeRefused: Deletable refused it on its live state (its tags or
	// class changed since the plan was made).
	OutcomeRefused Outcome = "refused"
	// OutcomeSkipped: something that depends on it was not removed.
	OutcomeSkipped Outcome = "skipped"
	// OutcomeFailed: Describe or Delete failed.
	OutcomeFailed Outcome = "failed"
	// OutcomeStopped: the lease heartbeat before it failed; execution
	// stopped here.
	OutcomeStopped Outcome = "stopped"
	// OutcomeNotRun: execution stopped before reaching it.
	OutcomeNotRun Outcome = "not-run"
)

// removed reports whether an outcome leaves the resource out of the way of
// what it depends on.
func (o Outcome) removed(mode Mode) bool {
	return o == OutcomeDeleted || o == OutcomeGone || (mode == ModeReport && o == OutcomeWouldDelete)
}

// Action is the record of one step.
type Action struct {
	ARN     string  `json:"arn"`
	Type    string  `json:"type"`
	Name    string  `json:"name"`
	Outcome Outcome `json:"outcome"`
	Reason  string  `json:"reason,omitempty"`
	// Err is the error behind a failed or stopped step.
	Err error `json:"-"`
}

// Result is what Execute did, one Action per plan step, in plan order.
type Result struct {
	Mode    Mode     `json:"mode"`
	Tool    string   `json:"tool"`
	Actions []Action `json:"actions"`
}

// Count returns how many actions had outcome o.
func (r *Result) Count(o Outcome) int {
	n := 0
	for _, a := range r.Actions {
		if a.Outcome == o {
			n++
		}
	}
	return n
}

// Execute runs plan. For each step, in order, it:
//
//  1. re-reads the resource with deleter.Describe and re-applies Deletable to
//     its live tags (and live name, class and ownership, where Describe
//     reports them);
//  2. calls lease.Step, which heartbeats the teardown lease and fails if the
//     lease was lost;
//  3. in Reap mode, calls deleter.Delete; in Report mode, records that it
//     would.
//
// A resource that is not found, at the re-check or the delete, counts as
// removed. A step that fails, or that Deletable refuses, does not stop the
// run, but every step waiting on it (what it depends on) is skipped. A failed
// lease.Step stops the run at once; the rest are recorded as not run.
//
// Reap mode needs a lease; Report mode may run without one (lease nil), for
// a footprint command outside any teardown. The returned error joins every
// failure, and wraps lifecycle.ErrLeaseLost if the lease was lost; the
// Result records every step either way.
func Execute(ctx context.Context, plan *Plan, mode Mode, deleter Deleter, lease *lifecycle.Lease) (*Result, error) {
	switch {
	case plan == nil:
		return nil, errors.New("footprint: nil plan")
	case mode != ModeReport && mode != ModeReap:
		return nil, fmt.Errorf("footprint: unknown mode %q", mode)
	case mode == ModeReap && lease == nil:
		return nil, errors.New("footprint: reap mode needs a teardown lease")
	case deleter == nil:
		return nil, errors.New("footprint: nil deleter")
	}
	res := &Result{Mode: mode, Tool: string(plan.Tool), Actions: make([]Action, len(plan.Steps))}
	blocked := make(map[int]string)
	var errs []error
	stopped := false
	for i, st := range plan.Steps {
		r := st.Resource
		a := &res.Actions[i]
		*a = Action{ARN: r.ARN, Type: r.Type, Name: r.Name}
		if stopped {
			a.Outcome, a.Reason = OutcomeNotRun, "execution stopped"
			continue
		}
		if err := ctx.Err(); err != nil {
			a.Outcome, a.Reason, a.Err = OutcomeNotRun, "execution stopped", err
			errs = append(errs, err)
			stopped = true
			continue
		}
		runStep(ctx, plan, mode, deleter, lease, r, a, blocked[i])
		switch a.Outcome {
		case OutcomeStopped:
			errs = append(errs, a.Err)
			stopped = true
		case OutcomeFailed:
			errs = append(errs, a.Err)
		}
		if !a.Outcome.removed(mode) {
			for _, j := range st.Then {
				if _, ok := blocked[j]; !ok {
					blocked[j] = fmt.Sprintf("%s depends on it and was %s", r.ARN, a.Outcome)
				}
			}
		}
	}
	return res, errors.Join(errs...)
}

// runStep fills a in for one step.
func runStep(ctx context.Context, plan *Plan, mode Mode, deleter Deleter, lease *lifecycle.Lease, r Resource, a *Action, blockedBy string) {
	if blockedBy != "" {
		a.Outcome, a.Reason = OutcomeSkipped, blockedBy
		return
	}
	if ok, why := Deletable(r, plan.Tool, plan.Patterns); !ok {
		a.Outcome, a.Reason = OutcomeKept, why
		return
	}
	fresh, err := deleter.Describe(ctx, r)
	if errors.Is(err, ErrNotFound) {
		a.Outcome, a.Reason = OutcomeGone, "not found at the re-check"
		return
	}
	if err != nil {
		a.Outcome, a.Reason, a.Err = OutcomeFailed, "re-check failed", fmt.Errorf("footprint: describe %s: %w", r.ARN, err)
		return
	}
	if ok, why := Deletable(live(r, fresh), plan.Tool, plan.Patterns); !ok {
		a.Outcome, a.Reason = OutcomeRefused, "changed since the plan: "+why
		return
	}
	if lease != nil {
		if err := lease.Step(ctx, "delete "+r.Type+" "+r.ARN); err != nil {
			a.Outcome, a.Reason, a.Err = OutcomeStopped, "teardown lease heartbeat failed", fmt.Errorf("footprint: before deleting %s: %w", r.ARN, err)
			return
		}
	}
	if mode == ModeReport {
		a.Outcome = OutcomeWouldDelete
		return
	}
	switch err := deleter.Delete(ctx, r); {
	case err == nil:
		a.Outcome = OutcomeDeleted
	case errors.Is(err, ErrNotFound):
		a.Outcome, a.Reason = OutcomeGone, "not found at the delete"
	default:
		a.Outcome, a.Reason, a.Err = OutcomeFailed, "delete failed", fmt.Errorf("footprint: delete %s: %w", r.ARN, err)
	}
}

// live is the planned resource with what Describe reported overlaid: always
// the tags (a resource Describe reports untagged is untagged), and the name,
// class and ownership when Describe sets them.
func live(planned, fresh Resource) Resource {
	r := planned
	r.Tags = fresh.Tags
	if fresh.Name != "" {
		r.Name = fresh.Name
	}
	if fresh.Class != "" {
		r.Class = fresh.Class
	}
	if fresh.Ownership != "" {
		r.Ownership = fresh.Ownership
	}
	return r
}
