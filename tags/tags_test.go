package tags

import (
	"testing"
	"time"
)

func TestFor(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 30, 15, 999, time.FixedZone("x", -5*3600))
	got := For(ToolSpawn, "sweep-42", now, "alice")
	want := map[string]string{
		ManagedBy: "spawn",
		Scope:     "sweep-42",
		Created:   "2026-11-16T15:30:15Z",
		Schema:    "1",
		Owner:     "alice",
	}
	if len(got) != len(want) {
		t.Fatalf("For = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("For[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestForOmitsEmptyScopeAndOwner(t *testing.T) {
	got := For(ToolLagotto, "", time.Unix(0, 0), "")
	if _, ok := got[Scope]; ok {
		t.Errorf("empty scope was set: %v", got)
	}
	if _, ok := got[Owner]; ok {
		t.Errorf("empty owner was set: %v", got)
	}
	if got[ManagedBy] != "lagotto" || got[Created] != "1970-01-01T00:00:00Z" {
		t.Errorf("For = %v", got)
	}
}

func TestForZeroTimeIsNow(t *testing.T) {
	before := time.Now().Add(-time.Second)
	got, ok := CreatedAt(For(ToolCalque, "s", time.Time{}, ""))
	if !ok || got.Before(before) || got.After(time.Now().Add(time.Second)) {
		t.Errorf("zero now produced Created %v (ok=%v), want about now", got, ok)
	}
}

func TestForReturnsFreshMap(t *testing.T) {
	a := For(ToolSpawn, "s", time.Unix(1, 0), "")
	a[Role] = RoleService
	if b := For(ToolSpawn, "s", time.Unix(1, 0), ""); IsService(b) {
		t.Error("For returned a shared map")
	}
}

func TestCreatedAt(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if got, ok := CreatedAt(map[string]string{Created: FormatCreated(ts)}); !ok || !got.Equal(ts) {
		t.Errorf("CreatedAt round trip = %v, %v", got, ok)
	}
	for _, m := range []map[string]string{nil, {}, {Created: "yesterday"}} {
		if _, ok := CreatedAt(m); ok {
			t.Errorf("CreatedAt(%v) ok, want not", m)
		}
	}
}

func TestIsManagedBy(t *testing.T) {
	m := For(ToolMCP, "", time.Unix(1, 0), "")
	if !IsManagedBy(m, ToolMCP) {
		t.Error("IsManagedBy(own tags) = false")
	}
	if IsManagedBy(m, ToolSpawn) {
		t.Error("IsManagedBy(other tool) = true")
	}
	if IsManagedBy(nil, ToolSpawn) {
		t.Error("IsManagedBy(nil) = true")
	}
}

func TestIsService(t *testing.T) {
	if IsService(nil) || IsService(map[string]string{Role: "worker"}) {
		t.Error("IsService true without Role=service")
	}
	if !IsService(map[string]string{Role: RoleService}) {
		t.Error("IsService false with Role=service")
	}
}

// The tag keys and tool names are written to AWS and read back by cleanup in
// other repos; changing one strands every resource tagged with the old value.
func TestWireValuesAreStable(t *testing.T) {
	for got, want := range map[string]string{
		ManagedBy: "spore-host:managed-by", Scope: "spore-host:scope",
		Created: "spore-host:created", Schema: "spore-host:schema",
		Owner: "spore-host:owner", Role: "spore-host:role",
		Component: "spore-host:component", SchemaVersion: "1", RoleService: "service",
		string(ToolSpawn): "spawn", string(ToolLagotto): "lagotto",
		string(ToolCalque): "calque", string(ToolTruffle): "truffle", string(ToolMCP): "spore-host-mcp",
	} {
		if got != want {
			t.Errorf("wire value %q, want %q", got, want)
		}
	}
}
