package catalog

import (
	"testing"
)

func TestLookup_ByName(t *testing.T) {
	e, ok := Lookup("paraview")
	if !ok {
		t.Fatal("Lookup(\"paraview\") = false, want true")
	}
	if e.Name != "paraview" {
		t.Errorf("Name = %q, want \"paraview\"", e.Name)
	}
	if !e.GPU {
		t.Error("GPU = false, want true")
	}
	if len(e.InstanceFamilies) == 0 {
		t.Error("InstanceFamilies is empty")
	}
}

func TestLookup_ByAlias(t *testing.T) {
	e, ok := Lookup("pv")
	if !ok {
		t.Fatal("Lookup(\"pv\") = false, want true")
	}
	if e.Name != "paraview" {
		t.Errorf("Name = %q, want \"paraview\"", e.Name)
	}
}

func TestLookup_CaseInsensitive(t *testing.T) {
	_, ok1 := Lookup("ParaView")
	_, ok2 := Lookup("PARAVIEW")
	_, ok3 := Lookup("paraview")
	if !ok1 || !ok2 || !ok3 {
		t.Error("Lookup should be case-insensitive")
	}
}

func TestLookup_Unknown(t *testing.T) {
	_, ok := Lookup("notarealapplication")
	if ok {
		t.Error("Lookup(unknown) = true, want false")
	}
}

func TestLookup_Alias_ImageJ(t *testing.T) {
	e, ok := Lookup("imagej")
	if !ok {
		t.Fatal("Lookup(\"imagej\") = false, want true")
	}
	if e.Name != "fiji" {
		t.Errorf("imagej alias → %q, want \"fiji\"", e.Name)
	}
}

func TestList_NotEmpty(t *testing.T) {
	apps := List()
	if len(apps) == 0 {
		t.Fatal("List() returned empty slice")
	}
}

func TestList_Sorted(t *testing.T) {
	apps := List()
	for i := 1; i < len(apps); i++ {
		if apps[i].Name < apps[i-1].Name {
			t.Errorf("List() not sorted: %q before %q", apps[i-1].Name, apps[i].Name)
		}
	}
}

// TestList_AllLaunchable asserts every app is launchable by exactly one model:
// a container image (CMD launches the app, #290) or a legacy launch_command
// (baked AMI). An app with neither can't be launched.
func TestList_AllLaunchable(t *testing.T) {
	for _, app := range List() {
		if app.Image == "" && app.LaunchCommand == "" {
			t.Errorf("app %q has neither image (container) nor launch_command (baked AMI)", app.Name)
		}
	}
}

// TestContainerApps_HaveBaseAMIAndTag asserts a containerized app carries the
// shared base-AMI table and a default tag — the two things the launch path needs.
func TestContainerApps_HaveBaseAMIAndTag(t *testing.T) {
	for _, app := range List() {
		if !app.Containerized() {
			continue
		}
		if app.TagDefault == "" {
			t.Errorf("container app %q has no tag_default", app.Name)
		}
		if app.BaseAMIs["us-east-1"] == "" {
			t.Errorf("container app %q has no us-east-1 base AMI", app.Name)
		}
	}
}

// TestNoDeprecatedPerAppAMIs guards against the #389 regression: no entry should
// reintroduce a baked per-app AMI table (superseded by image + base_amis).
func TestNoDeprecatedPerAppAMIs(t *testing.T) {
	for _, app := range List() {
		if len(app.AMIs) != 0 {
			t.Errorf("app %q sets deprecated per-app amis: %v — use image + base_amis (#290/#389)", app.Name, app.AMIs)
		}
	}
}

func TestResolveTag(t *testing.T) {
	e := &AppEntry{Name: "paraview", TagDefault: "5.13.2", TagsAvailable: []string{"5.13.2", "5.12.1"}}
	tests := []struct {
		name      string
		requested string
		want      string
		wantErr   bool
	}{
		{"empty → default", "", "5.13.2", false},
		{"explicit default", "5.13.2", "5.13.2", false},
		{"available alt", "5.12.1", "5.12.1", false},
		{"unavailable", "9.9.9", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := e.ResolveTag(tt.requested)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ResolveTag(%q) err = %v, wantErr %v", tt.requested, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ResolveTag(%q) = %q, want %q", tt.requested, got, tt.want)
			}
		})
	}
}

func TestResolveTag_NoDefault(t *testing.T) {
	e := &AppEntry{Name: "x"}
	if _, err := e.ResolveTag(""); err == nil {
		t.Error("ResolveTag(\"\") with no TagDefault should error")
	}
}

func TestList_GPUAppsHaveVRAM(t *testing.T) {
	for _, app := range List() {
		if app.GPU && app.MinVRAMGiB == 0 {
			t.Errorf("GPU app %q has MinVRAMGiB=0", app.Name)
		}
	}
}
