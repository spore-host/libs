package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMergeApps(t *testing.T) {
	base := []AppEntry{
		{Name: "paraview", Image: "public.ecr.aws/x/paraview", TagDefault: "5.13.2"},
		{Name: "igv", LaunchCommand: "/opt/igv/igv.sh"},
	}
	over := []AppEntry{
		{Name: "paraview", Image: "123456789012.dkr.ecr.us-east-1.amazonaws.com/paraview", TagDefault: "5.13.2"}, // rebind
		{Name: "myapp", Image: "public.ecr.aws/me/myapp", TagDefault: "1.0"},                                     // new
	}
	got := mergeApps(base, over)

	if len(got) != 3 {
		t.Fatalf("merged len = %d, want 3 (paraview rebound, igv kept, myapp added)", len(got))
	}
	byName := map[string]AppEntry{}
	for _, e := range got {
		byName[e.Name] = e
	}
	pvMerged := byName["paraview"]
	if pvMerged.Image != "123456789012.dkr.ecr.us-east-1.amazonaws.com/paraview" {
		t.Errorf("paraview not rebound by overlay: %q", pvMerged.Image)
	}
	if pvMerged.ImageVisibility() != VisibilityPrivate {
		t.Errorf("rebound paraview should be private, got %s", pvMerged.ImageVisibility())
	}
	if byName["igv"].LaunchCommand != "/opt/igv/igv.sh" {
		t.Errorf("igv (untouched) lost its launch command")
	}
	if _, ok := byName["myapp"]; !ok {
		t.Errorf("overlay-only app myapp missing from merge")
	}
}

func TestMergeApps_CaseInsensitiveName(t *testing.T) {
	base := []AppEntry{{Name: "ParaView", Image: "public.ecr.aws/x/pv"}}
	over := []AppEntry{{Name: "paraview", Image: "public.ecr.aws/me/pv"}}
	got := mergeApps(base, over)
	if len(got) != 1 {
		t.Fatalf("case-different names should merge to 1 entry, got %d", len(got))
	}
	if got[0].Image != "public.ecr.aws/me/pv" {
		t.Errorf("overlay should win case-insensitively, got %q", got[0].Image)
	}
}

// withOverlay points the catalog at a temp overlay file, reloads, and restores
// the embedded-only state on cleanup. Serializes via the package state, so these
// tests must not run in parallel with other catalog tests (they don't).
func withOverlay(t *testing.T, yaml string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write overlay: %v", err)
	}
	SetOverlayPath(path)
	Reload()
	t.Cleanup(func() {
		SetOverlayPath("")
		Reload()
	})
}

func TestOverlay_AddsAndRebinds(t *testing.T) {
	withOverlay(t, `
apps:
  - name: paraview
    description: "my private paraview"
    image: 123456789012.dkr.ecr.us-east-1.amazonaws.com/paraview
    tag_default: "5.13.2"
    gpu: true
  - name: mytool
    description: "a tool only I have"
    image: public.ecr.aws/me/mytool
    tag_default: "2.0"
`)

	// Rebound existing app.
	pv, ok := Lookup("paraview")
	if !ok {
		t.Fatal("paraview missing after overlay")
	}
	if pv.Image != "123456789012.dkr.ecr.us-east-1.amazonaws.com/paraview" {
		t.Errorf("paraview not rebound by overlay: %q", pv.Image)
	}
	if pv.ImageVisibility() != VisibilityPrivate {
		t.Errorf("rebound paraview should be private, got %s", pv.ImageVisibility())
	}

	// Overlay-only app is present.
	if _, ok := Lookup("mytool"); !ok {
		t.Error("overlay-only app mytool not found via Lookup")
	}

	// Untouched embedded app survives (igv ships in the embedded catalog).
	if _, ok := Lookup("igv"); !ok {
		t.Error("embedded app igv lost after overlay merge")
	}

	if err := LoadError(); err != nil {
		t.Errorf("unexpected overlay load error: %v", err)
	}
}

func TestOverlay_MissingExplicitPathIsError(t *testing.T) {
	SetOverlayPath(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	Reload()
	t.Cleanup(func() { SetOverlayPath(""); Reload() })

	if LoadError() == nil {
		t.Error("an explicitly-set but missing overlay path should report LoadError")
	}
	// Embedded catalog must still be usable despite the bad overlay.
	if _, ok := Lookup("igv"); !ok {
		t.Error("embedded catalog should remain available when overlay is missing")
	}
}

func TestOverlay_MalformedIsNonFatal(t *testing.T) {
	withOverlay(t, "this: is: not: valid: yaml: [")
	if LoadError() == nil {
		t.Error("malformed overlay should report LoadError")
	}
	if _, ok := Lookup("igv"); !ok {
		t.Error("embedded catalog should remain available when overlay is malformed")
	}
}
