package lifecycle_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/lifecycle/lifecycletest"
	"github.com/spore-host/libs/tags"
)

// This file runs creators, holders and deleters (backstops) as goroutines
// against one shared lifecycletest.Fake, under a seeded scheduler that decides
// the interleaving, and checks the protocol's invariants after every step.
//
// The scheduler lets exactly one worker run at a time. A worker parks at every
// DynamoDB call (the fake's Hook) and at every Sleep; the scheduler then picks
// one runnable worker at random (seeded) and advances a simulated clock by a
// random 0-2 s. A Sleep(d) makes its worker runnable again only once the clock
// reaches its wake time; when nobody is runnable the clock jumps to the next
// wake time. Every seed is a different, reproducible interleaving.
//
// Invariants, checked as events happen:
//
//	(a)  no MarkActive/AddHold succeeds while a deleter is in its deleting
//	     window (after its post-claim re-check, until Finish or lease loss);
//	     checked when leases cannot go stale
//	(a') no delete step runs while a creator is mid-work or a hold exists
//	(b)  fencing: once a creator (or a newer claim) has succeeded after a
//	     deleter's Claim, that deleter's Step and Finish never succeed again,
//	     so it ends in Release or ErrLeaseLost
//	(c)  the table is never deleted while it holds a tool item
//	(d)  a Claim succeeds only once grace has passed since the tool's last
//	     successful MarkActive or AddHold
//	(e)  no Claim succeeds while a hold exists
//
// LIFECYCLE_SIM_SEEDS sets the number of seeds per variant.

type simWorkerKey struct{}

type arrival struct {
	id   int
	at   time.Time
	done bool
}

type sim struct {
	t    *testing.T
	seed int64
	rng  *rand.Rand

	mu  sync.Mutex
	now time.Time

	arrive chan arrival
	wake   []chan struct{}
}

func (s *sim) clock() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

// yield parks the calling worker until the scheduler picks it again, no
// sooner than d from now. Calls outside a worker (none, in practice) pass.
func (s *sim) yield(ctx context.Context, d time.Duration) {
	id, ok := ctx.Value(simWorkerKey{}).(int)
	if !ok {
		return
	}
	s.arrive <- arrival{id: id, at: s.clock().Add(d)}
	<-s.wake[id]
}

func workerID(ctx context.Context) int { return ctx.Value(simWorkerKey{}).(int) }

func (s *sim) run(workers []func(ctx context.Context)) {
	n := len(workers)
	s.arrive = make(chan arrival)
	s.wake = make([]chan struct{}, n)
	for i := range s.wake {
		s.wake[i] = make(chan struct{})
	}
	for i, w := range workers {
		go func() {
			ctx := context.WithValue(context.Background(), simWorkerKey{}, i)
			s.yield(ctx, 0)
			defer func() {
				if r := recover(); r != nil {
					s.t.Errorf("seed %d: worker %d panicked: %v", s.seed, i, r)
				}
				s.arrive <- arrival{id: i, done: true}
			}()
			w(ctx)
		}()
	}
	parked := make([]bool, n)
	at := make([]time.Time, n)
	for range n {
		a := <-s.arrive
		parked[a.id], at[a.id] = true, a.at
	}
	live := n
	for live > 0 {
		now := s.clock()
		var runnable []int
		var next time.Time
		for i := range n {
			if !parked[i] {
				continue
			}
			if !at[i].After(now) {
				runnable = append(runnable, i)
			} else if next.IsZero() || at[i].Before(next) {
				next = at[i]
			}
		}
		if len(runnable) == 0 {
			s.mu.Lock()
			s.now = next
			s.mu.Unlock()
			continue
		}
		id := runnable[s.rng.Intn(len(runnable))]
		s.mu.Lock()
		s.now = s.now.Add(time.Duration(s.rng.Intn(2000)) * time.Millisecond)
		s.mu.Unlock()
		parked[id] = false
		s.wake[id] <- struct{}{}
		a := <-s.arrive
		if a.done {
			live--
		} else {
			parked[a.id], at[a.id] = true, a.at
		}
	}
}

// model is the ground truth the invariants are checked against.
type model struct {
	mu         sync.Mutex
	strict     bool // leases cannot go stale: check (a)
	grace      time.Duration
	active     map[tags.Tool]int
	holds      map[tags.Tool]int
	lastActive map[tags.Tool]time.Time
	window     map[int]tags.Tool // deleter id -> tool, while deleting
	newest     map[tags.Tool]int // newest successful claimant per tool; -1 after a creator
	violations []string
	counts     map[string]int
}

func newModel(strict bool, grace time.Duration) *model {
	return &model{
		strict: strict, grace: grace,
		active: map[tags.Tool]int{}, holds: map[tags.Tool]int{}, lastActive: map[tags.Tool]time.Time{},
		window: map[int]tags.Tool{}, newest: map[tags.Tool]int{}, counts: map[string]int{},
	}
}

func (m *model) violate(format string, args ...any) {
	m.violations = append(m.violations, fmt.Sprintf(format, args...))
}

func (m *model) count(k string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[k]++
}

// activated records a successful MarkActive or AddHold.
func (m *model) activated(tl tags.Tool, now time.Time, hold bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.strict {
		for id, wt := range m.window {
			if wt == tl {
				m.violate("(a) %s activation succeeded while deleter %d is deleting", tl, id)
			}
		}
	}
	m.newest[tl] = -1
	m.lastActive[tl] = now
	if hold {
		m.holds[tl]++
		m.counts["hold"]++
	} else {
		m.active[tl]++
		m.counts["create"]++
	}
}

func (m *model) workDone(tl tags.Tool, hold bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if hold {
		m.holds[tl]--
	} else {
		m.active[tl]--
	}
}

func (m *model) isActive(tl tags.Tool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active[tl] > 0
}

func (m *model) claimed(id int, tl tags.Tool, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts["claim"]++
	if last, ok := m.lastActive[tl]; ok && now.Sub(last) < m.grace {
		m.violate("(d) deleter %d claimed %s %s after its last activation (grace %s)", id, tl, now.Sub(last), m.grace)
	}
	if m.holds[tl] > 0 {
		m.violate("(e) deleter %d claimed %s while it has %d holds", id, tl, m.holds[tl])
	}
	m.newest[tl] = id
}

func (m *model) openWindow(id int, tl tags.Tool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.window[id] = tl
}

// fenced checks that a successful Step or Finish belongs to the newest claim.
func (m *model) fenced(id int, tl tags.Tool, what string) {
	if m.newest[tl] != id {
		m.violate("(b) deleter %d's %s on %s succeeded after a newer activation or claim (%d)", id, what, tl, m.newest[tl])
	}
}

func (m *model) deleting(id int, tl tags.Tool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts["delete"]++
	m.fenced(id, tl, "Step")
	if m.active[tl] > 0 || m.holds[tl] > 0 {
		m.violate("(a') deleter %d deleting %s while %d creators are working and %d holds exist", id, tl, m.active[tl], m.holds[tl])
	}
}

func (m *model) end(id int, tl tags.Tool, outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[outcome]++
	if outcome == "finish" {
		m.fenced(id, tl, "Finish")
	}
	delete(m.window, id)
}

type simConfig struct {
	name       string
	strict     bool
	staleAfter time.Duration
	pauseProb  float64 // chance a deleter stalls past staleAfter before a Step
}

func simSeeds(t *testing.T) int {
	if v := os.Getenv("LIFECYCLE_SIM_SEEDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("LIFECYCLE_SIM_SEEDS=%q", v)
		}
		return n
	}
	if testing.Short() {
		return 150
	}
	return 1500
}

func TestConcurrentCreatorsAndDeleters(t *testing.T) {
	seeds := simSeeds(t)
	for _, cfg := range []simConfig{
		{name: "live leases", strict: true, staleAfter: lifecycle.DefaultStaleAfter},
		{name: "stalling deleters", staleAfter: 60 * time.Second, pauseProb: 0.25},
	} {
		t.Run(cfg.name, func(t *testing.T) {
			total := map[string]int{}
			for seed := int64(1); seed <= int64(seeds); seed++ {
				m := runSim(t, cfg, seed)
				for k, v := range m.counts {
					total[k] += v
				}
				if len(m.violations) > 0 {
					t.Fatalf("seed %d (%s): %d violations, first: %s", seed, cfg.name, len(m.violations), m.violations[0])
				}
			}
			keys := make([]string, 0, len(total))
			for k := range total {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			t.Logf("%d interleavings; outcomes:", seeds)
			for _, k := range keys {
				t.Logf("  %-16s %d", k, total[k])
			}
			// Every outcome the protocol allows must actually have happened,
			// or the interleavings are not exercising it.
			want := []string{"create", "hold", "claim", "delete", "finish", "release", "creator-waited", "closed", "close-refused", "table-closing"}
			if !cfg.strict {
				want = append(want, "lost", "stall")
			}
			for _, k := range want {
				if total[k] == 0 && seeds >= 100 {
					t.Errorf("outcome %q never happened in %d interleavings", k, seeds)
				}
			}
		})
	}
}

func runSim(t *testing.T, cfg simConfig, seed int64) *model {
	const (
		grace = 2 * time.Minute
		scope = region
	)
	s := &sim{t: t, seed: seed, rng: rand.New(rand.NewSource(seed)), now: t0}
	m := newModel(cfg.strict, grace)
	f := lifecycletest.New()
	f.Hook = func(ctx context.Context, _ string) { s.yield(ctx, 0) }

	client := func(owner string, id int) *lifecycle.Client {
		c, err := lifecycle.New(lifecycle.Options{
			DynamoDB: func(string) (lifecycle.API, error) { return f, nil },
			Owner:    owner,
			Now:      s.clock,
			Rand:     rand.New(rand.NewSource(seed*100 + int64(id))),
			Sleep: func(ctx context.Context, d time.Duration) error {
				if d == lifecycle.DefaultPollInterval {
					m.count("creator-waited")
				}
				s.yield(ctx, d)
				return nil
			},
			StaleAfter: cfg.staleAfter,
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	wrng := func(ctx context.Context) *rand.Rand {
		return rand.New(rand.NewSource(seed*1000 + int64(workerID(ctx))))
	}
	jitter := func(r *rand.Rand, maxSec int) time.Duration {
		return time.Duration(r.Intn(maxSec*1000+1)) * time.Millisecond
	}
	unexpected := func(who string, err error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.violate("%s: unexpected error: %v", who, err)
	}

	creator := func(tl tags.Tool, rounds int) func(ctx context.Context) {
		return func(ctx context.Context) {
			r := wrng(ctx)
			c := client(fmt.Sprintf("creator-%d", workerID(ctx)), workerID(ctx))
			for range rounds {
				s.yield(ctx, jitter(r, 400))
				err := c.MarkActive(ctx, tl, scope)
				if errors.Is(err, lifecycle.ErrWaitTimeout) {
					m.count("creator-timeout")
					continue
				}
				if err != nil {
					unexpected("creator", err)
					return
				}
				m.activated(tl, s.clock(), false)
				for range 1 + r.Intn(3) {
					s.yield(ctx, jitter(r, 120)) // creating, then using, resources
				}
				m.workDone(tl, false)
			}
		}
	}
	holder := func(tl, by tags.Tool, rounds int) func(ctx context.Context) {
		return func(ctx context.Context) {
			r := wrng(ctx)
			c := client("holder", workerID(ctx))
			for range rounds {
				s.yield(ctx, jitter(r, 500))
				err := c.AddHold(ctx, tl, scope, by)
				if errors.Is(err, lifecycle.ErrWaitTimeout) {
					continue
				}
				if err != nil {
					unexpected("holder", err)
					return
				}
				m.activated(tl, s.clock(), true)
				s.yield(ctx, jitter(r, 200))
				if err := c.RemoveHold(ctx, tl, scope, by); err != nil {
					unexpected("holder", err)
					return
				}
				m.workDone(tl, true)
			}
		}
	}
	deleter := func(tl tags.Tool, ticks int) func(ctx context.Context) {
		return func(ctx context.Context) {
			r := wrng(ctx)
			id := workerID(ctx)
			c := client(fmt.Sprintf("backstop-%d", id), id)
			for range ticks {
				s.yield(ctx, 20*time.Second+jitter(r, 40))
				// A backstop's view of activity can lag (listing APIs are
				// eventually consistent): sometimes it misses a live signal.
				// Its post-claim re-check below is always accurate.
				seen := m.isActive(tl) && r.Float64() < 0.5
				_, idle, err := c.Observe(ctx, tl, scope, seen)
				if errors.Is(err, lifecycle.ErrTableClosing) {
					m.count("table-closing")
					continue
				}
				if err != nil {
					unexpected("observe", err)
					return
				}
				if idle < grace {
					continue
				}
				l, err := c.Claim(ctx, tl, scope, grace)
				if errors.Is(err, lifecycle.ErrNotClaimable) {
					continue
				}
				if err != nil {
					unexpected("claim", err)
					return
				}
				m.claimed(id, tl, s.clock())
				s.yield(ctx, jitter(r, 5))
				if m.isActive(tl) { // the post-claim re-check
					switch err := l.Release(ctx); {
					case err == nil:
						m.end(id, tl, "release")
					case errors.Is(err, lifecycle.ErrLeaseLost):
						m.end(id, tl, "lost")
					default:
						unexpected("release", err)
						return
					}
					continue
				}
				m.openWindow(id, tl)
				lost := false
				for step := range 1 + r.Intn(4) {
					if cfg.pauseProb > 0 && r.Float64() < cfg.pauseProb {
						m.count("stall")
						s.yield(ctx, cfg.staleAfter+jitter(r, 60))
					}
					err := l.Step(ctx, fmt.Sprintf("delete %d", step))
					if errors.Is(err, lifecycle.ErrLeaseLost) {
						m.end(id, tl, "lost")
						lost = true
						break
					}
					if err != nil {
						unexpected("step", err)
						return
					}
					m.deleting(id, tl)
					s.yield(ctx, jitter(r, 5)) // the delete call
				}
				if lost {
					continue
				}
				switch err := l.Finish(ctx); {
				case errors.Is(err, lifecycle.ErrLeaseLost):
					m.end(id, tl, "lost")
					continue
				case err != nil:
					unexpected("finish", err)
					return
				}
				m.end(id, tl, "finish")
				gone, err := c.CloseTableIfEmpty(ctx, region)
				switch {
				case err != nil:
					unexpected("close", err)
					return
				case gone:
					m.count("closed")
				default:
					m.count("close-refused")
				}
			}
		}
	}

	s.run([]func(context.Context){
		creator(tags.ToolLagotto, 3),
		creator(tags.ToolLagotto, 3),
		creator(tags.ToolCalque, 3),
		holder(tags.ToolLagotto, tags.ToolCalque, 2),
		deleter(tags.ToolLagotto, 30),
		deleter(tags.ToolLagotto, 30),
		deleter(tags.ToolCalque, 30),
	})

	// (c): every DeleteTable found nothing but the sentinel.
	for _, d := range f.Deletions() {
		for _, k := range d.Keys {
			if k != lifecycle.SentinelKey {
				m.violate("(c) table deleted while it held %s", k)
			}
		}
	}
	return m
}
