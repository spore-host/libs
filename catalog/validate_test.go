package catalog

import (
	"strings"
	"testing"
)

// TestValidate_EmbeddedCatalogClean is the CI gate: the shipped catalog.yaml must
// pass validation. A failure here means a bad entry would reach production (the
// #389 class of bug).
func TestValidate_EmbeddedCatalogClean(t *testing.T) {
	if errs := Validate(); len(errs) != 0 {
		for _, e := range errs {
			t.Errorf("catalog invalid: %v", e)
		}
	}
}

func TestValidateApps_CatchesDefects(t *testing.T) {
	base := map[string]string{"us-east-1": "ami-123"}
	tests := []struct {
		name    string
		apps    []AppEntry
		wantSub string // substring expected in the (single) error
	}{
		{
			name:    "not launchable",
			apps:    []AppEntry{{Name: "x"}},
			wantSub: "not launchable",
		},
		{
			name:    "deprecated per-app amis (#389)",
			apps:    []AppEntry{{Name: "x", LaunchCommand: "/bin/x", AMIs: map[string]string{"us-east-1": "ami-9"}}},
			wantSub: "deprecated per-app amis",
		},
		{
			name:    "container without tag_default",
			apps:    []AppEntry{{Name: "x", Image: "ecr/x", BaseAMIs: base}},
			wantSub: "no tag_default",
		},
		{
			name:    "tag_default not in tags_available",
			apps:    []AppEntry{{Name: "x", Image: "ecr/x", TagDefault: "9.9", TagsAvailable: []string{"1.0"}, BaseAMIs: base}},
			wantSub: "not in tags_available",
		},
		{
			name:    "container without base_amis",
			apps:    []AppEntry{{Name: "x", Image: "ecr/x", TagDefault: "1.0"}},
			wantSub: "no base_amis",
		},
		{
			name:    "base_amis all empty",
			apps:    []AppEntry{{Name: "x", Image: "ecr/x", TagDefault: "1.0", BaseAMIs: map[string]string{"us-east-1": ""}}},
			wantSub: "no base_amis",
		},
		{
			name: "two apps share an image",
			apps: []AppEntry{
				{Name: "a", Image: "ecr/shared", TagDefault: "1", BaseAMIs: base},
				{Name: "b", Image: "ecr/shared", TagDefault: "1", BaseAMIs: base},
			},
			wantSub: "also used by",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validateApps(tt.apps)
			if len(errs) == 0 {
				t.Fatalf("expected an error containing %q, got none", tt.wantSub)
			}
			joined := ""
			for _, e := range errs {
				joined += e.Error() + "\n"
			}
			if !strings.Contains(joined, tt.wantSub) {
				t.Errorf("errors %q do not contain %q", joined, tt.wantSub)
			}
		})
	}
}

func TestValidateApps_AcceptsGoodEntries(t *testing.T) {
	base := map[string]string{"us-east-1": "ami-123"}
	apps := []AppEntry{
		{Name: "paraview", Image: "ecr/paraview", TagDefault: "5.13.2", TagsAvailable: []string{"5.13.2"}, BaseAMIs: base},
		{Name: "igv", LaunchCommand: "/opt/igv/igv.sh"}, // legacy CPU app, still valid
	}
	if errs := validateApps(apps); len(errs) != 0 {
		t.Errorf("valid apps reported errors: %v", errs)
	}
}
