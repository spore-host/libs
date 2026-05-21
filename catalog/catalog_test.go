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

func TestList_AllHaveLaunchCommand(t *testing.T) {
	for _, app := range List() {
		if app.LaunchCommand == "" {
			t.Errorf("app %q has empty LaunchCommand", app.Name)
		}
	}
}

func TestList_GPUAppsHaveVRAM(t *testing.T) {
	for _, app := range List() {
		if app.GPU && app.MinVRAMGiB == 0 {
			t.Errorf("GPU app %q has MinVRAMGiB=0", app.Name)
		}
	}
}
