package catalog

import (
	"os"
	"testing"
)

// TestMain isolates catalog tests from any AMBIENT overlay on the developer's
// machine (a real ~/.spawn/catalog.yaml or $SPAWN_CATALOG) so embedded-catalog
// assertions (e.g. Validate, recipe-only checks) test the shipped data, not the
// local environment. Tests that exercise the overlay set their own path via
// withOverlay/SetOverlayPath and restore on cleanup.
func TestMain(m *testing.M) {
	os.Unsetenv("SPAWN_CATALOG")
	// Point HOME at an empty dir so ~/.spawn/catalog.yaml doesn't resolve.
	os.Setenv("HOME", os.TempDir()+"/spore-catalog-test-home-empty")
	SetOverlayPath("")
	Reload()
	os.Exit(m.Run())
}
