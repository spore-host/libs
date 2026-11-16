package lifecycle_test

import (
	"context"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/lifecycle/lifecycletest"
	"github.com/spore-host/libs/tags"
)

const (
	region  = "us-west-2"
	control = "us-east-1"
	tool    = tags.ToolLagotto
	grace   = 24 * time.Hour
)

var t0 = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

// env is one test's world: a fake DynamoDB per region and a fake clock that
// Sleep advances.
type env struct {
	t      *testing.T
	mu     sync.Mutex
	fakes  map[string]*lifecycletest.Fake
	now    time.Time
	sleeps []time.Duration
	// onSleep, if set, runs after each sleep with its 1-based count.
	onSleep func(n int)
	c       *lifecycle.Client
}

func newEnv(t *testing.T, mod ...func(*lifecycle.Options)) *env {
	t.Helper()
	e := &env{t: t, fakes: map[string]*lifecycletest.Fake{region: lifecycletest.New(), control: lifecycletest.New()}, now: t0}
	e.c = e.client("tester", mod...)
	return e
}

// client returns another participant on the same fakes and clock.
func (e *env) client(owner string, mod ...func(*lifecycle.Options)) *lifecycle.Client {
	e.t.Helper()
	o := lifecycle.Options{
		DynamoDB: func(r string) (lifecycle.API, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.fakes[r], nil
		},
		ControlRegion: control,
		Owner:         owner,
		Now:           e.clock,
		Rand:          rand.New(rand.NewSource(int64(len(owner)))),
		Sleep: func(ctx context.Context, d time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			e.mu.Lock()
			e.now = e.now.Add(d)
			e.sleeps = append(e.sleeps, d)
			n, f := len(e.sleeps), e.onSleep
			e.mu.Unlock()
			if f != nil {
				f(n)
			}
			return nil
		},
	}
	for _, m := range mod {
		m(&o)
	}
	c, err := lifecycle.New(o)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *env) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = e.now.Add(d)
}

func (e *env) fake() *lifecycletest.Fake { return e.fakes[region] }

func (e *env) item(t *testing.T) lifecycle.Item {
	t.Helper()
	it, ok, err := e.c.Get(context.Background(), tool, region)
	if err != nil || !ok {
		t.Fatalf("Get = %v, %v", ok, err)
	}
	return it
}

// idleAndClaim brings the tool to a claimed lease: active, idle for grace,
// claimed.
func (e *env) idleAndClaim(t *testing.T) *lifecycle.Lease {
	t.Helper()
	ctx := context.Background()
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.c.Observe(ctx, tool, region, false); err != nil {
		t.Fatal(err)
	}
	e.advance(grace)
	l, err := e.c.Claim(ctx, tool, region, grace)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return l
}

func sAttr(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
