package lifecycle_test

import (
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/tags"
)

func TestLeaseStale(t *testing.T) {
	e := newEnv(t)
	if e.c.LeaseStale(lifecycle.Item{}) {
		t.Error("an item with no lease is stale")
	}
	e.idleAndClaim(t)
	if e.c.LeaseStale(e.item(t)) {
		t.Error("a fresh lease is stale")
	}
	e.advance(lifecycle.DefaultStaleAfter)
	if e.c.LeaseStale(e.item(t)) {
		t.Error("a lease exactly StaleAfter old is stale (Claim and MarkActive treat it as live)")
	}
	e.advance(time.Millisecond)
	if !e.c.LeaseStale(e.item(t)) {
		t.Error("a lease past StaleAfter is not stale")
	}
	if !e.c.LeaseStale(lifecycle.Item{Lease: &lifecycle.LeaseInfo{Owner: "x"}}) {
		t.Error("a lease with no heartbeat is not stale")
	}
}

func TestListItems(t *testing.T) {
	e := newEnv(t)
	items, exists, err := e.c.ListItems(ctx, region)
	if err != nil || exists || len(items) != 0 {
		t.Fatalf("ListItems with no table = %v, %v, %v", items, exists, err)
	}
	for _, tl := range []tags.Tool{tags.ToolSpawn, tags.ToolLagotto, tags.ToolCalque} {
		if err := e.c.MarkActive(ctx, tl, region); err != nil {
			t.Fatal(err)
		}
	}
	putSentinel(t, e, e.clock())
	e.fake().ScanPage = 1
	writes := e.fake().Calls("UpdateItem") + e.fake().Calls("PutItem") + e.fake().Calls("TransactWriteItems") + e.fake().Calls("DeleteItem")
	items, exists, err = e.c.ListItems(ctx, region)
	if err != nil || !exists {
		t.Fatalf("ListItems = %v, %v", exists, err)
	}
	if len(items) != 3 {
		t.Fatalf("ListItems = %d items, want 3 (sentinel excluded): %+v", len(items), items)
	}
	for _, it := range items {
		if it.Scope != region || it.State != lifecycle.StateActive {
			t.Errorf("item %+v", it)
		}
	}
	if got := e.fake().Calls("UpdateItem") + e.fake().Calls("PutItem") + e.fake().Calls("TransactWriteItems") + e.fake().Calls("DeleteItem"); got != writes {
		t.Errorf("ListItems wrote: %d writes, want %d", got, writes)
	}

	e.fake().SetStatus(lifecycle.TableName, types.TableStatusDeleting, 5)
	if _, exists, err := e.c.ListItems(ctx, region); err != nil || exists {
		t.Errorf("ListItems on a DELETING table = %v, %v; want not exists", exists, err)
	}
}

func TestListItemsErrors(t *testing.T) {
	e := newEnv(t)
	if err := e.c.MarkActive(ctx, tool, region); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("throttled")
	e.fake().Err = func(op string, _ any) error {
		if op == "Scan" {
			return boom
		}
		return nil
	}
	if _, _, err := e.c.ListItems(ctx, region); !errors.Is(err, boom) {
		t.Errorf("ListItems = %v, want %v", err, boom)
	}
	if _, _, err := e.c.ListItems(ctx, ""); err == nil {
		t.Error("ListItems with no region succeeded")
	}
}
