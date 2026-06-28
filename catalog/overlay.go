package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Local catalog overlay (BYO-image model, #392).
//
// The embedded catalog is the global, public baseline shipped to everyone. A
// user can layer a local overlay on top to add their own apps or rebind an
// existing app's image to one they host (public or private). Overlay entries are
// the only place private images belong — the shipped catalog stays public-only.
//
// Resolution precedence for the overlay path: an explicit path set via
// SetOverlayPath (e.g. a --catalog flag) > $SPAWN_CATALOG > ~/.spawn/catalog.yaml.
// A missing file is not an error (overlay is opt-in); a malformed one is reported
// via LoadError and the catalog falls back to embedded-only.

// overlayEnvVar is the environment variable naming an overlay catalog file.
const overlayEnvVar = "SPAWN_CATALOG"

// defaultOverlayRel is the overlay path under the user's home directory.
var defaultOverlayRel = filepath.Join(".spawn", "catalog.yaml")

// explicitOverlayPath, when non-empty, overrides the env/default lookup. Guarded
// by mu (set under SetOverlayPath, read under build).
var explicitOverlayPath string

// SetOverlayPath sets an explicit overlay file path (highest precedence), e.g.
// from a --catalog flag. Pass "" to clear it and fall back to env/default. Call
// Reload afterward to apply.
func SetOverlayPath(path string) {
	mu.Lock()
	defer mu.Unlock()
	explicitOverlayPath = path
}

// overlayPath returns the overlay file path per precedence, or "" if none is
// configured/discoverable. Caller holds mu.
func overlayPath() string {
	if explicitOverlayPath != "" {
		return explicitOverlayPath
	}
	if p := os.Getenv(overlayEnvVar); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, defaultOverlayRel)
}

// loadOverlayApps reads and parses the overlay file. Returns (nil, nil) when no
// overlay path is configured or the default file is simply absent (opt-in), and
// (nil, err) when a configured file can't be read or parsed.
func loadOverlayApps() ([]AppEntry, error) {
	path := overlayPath()
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Absent default/env file is fine; an absent EXPLICIT path is an error
			// (the user asked for it).
			if explicitOverlayPath != "" {
				return nil, fmt.Errorf("catalog overlay %q: %w", path, err)
			}
			return nil, nil
		}
		return nil, fmt.Errorf("catalog overlay %q: %w", path, err)
	}
	var f catalogFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("catalog overlay %q: parse: %w", path, err)
	}
	return f.Apps, nil
}

// mergeApps overlays `over` onto `base`, returning a new slice. An overlay entry
// whose Name matches a base entry is FIELD-MERGED onto it: each non-zero overlay
// field overrides the base, and unset overlay fields inherit the base value — so
// binding just an image to a recipe-only app keeps the app's description, GPU,
// families, etc. An overlay entry with a new Name is appended as-is. Pure —
// unit-tested without the filesystem. Order is not significant (caller sorts).
func mergeApps(base, over []AppEntry) []AppEntry {
	idx := make(map[string]int, len(base)) // lowercased name → index in result
	out := make([]AppEntry, len(base))
	copy(out, base)
	for i := range out {
		idx[strings.ToLower(out[i].Name)] = i
	}
	for _, e := range over {
		if i, ok := idx[strings.ToLower(e.Name)]; ok {
			out[i] = mergeEntry(out[i], e)
		} else {
			idx[strings.ToLower(e.Name)] = len(out)
			out = append(out, e)
		}
	}
	return out
}

// mergeEntry returns base with every non-zero field of over applied on top.
// Used when an overlay rebinds an existing app: the overlay typically supplies
// just an image binding, and the rest of the definition is inherited.
//
// Note: binding an image clears the recipe-only state via Image, but a rebind
// does NOT auto-clear a stale Recipe pointer (harmless — it's just a doc link).
func mergeEntry(base, over AppEntry) AppEntry {
	m := base
	if over.Description != "" {
		m.Description = over.Description
	}
	if over.InstanceFamilies != nil {
		m.InstanceFamilies = over.InstanceFamilies
	}
	if over.HighVRAMFamilies != nil {
		m.HighVRAMFamilies = over.HighVRAMFamilies
	}
	if over.MinVCPUs != 0 {
		m.MinVCPUs = over.MinVCPUs
	}
	if over.MinMemoryGiB != 0 {
		m.MinMemoryGiB = over.MinMemoryGiB
	}
	if over.GPU {
		m.GPU = over.GPU
	}
	if over.MinVRAMGiB != 0 {
		m.MinVRAMGiB = over.MinVRAMGiB
	}
	if over.DCVEnabled {
		m.DCVEnabled = over.DCVEnabled
	}
	if over.IdleTimeoutDefault != "" {
		m.IdleTimeoutDefault = over.IdleTimeoutDefault
	}
	if over.LaunchCommand != "" {
		m.LaunchCommand = over.LaunchCommand
	}
	if over.Aliases != nil {
		m.Aliases = over.Aliases
	}
	if over.License != "" {
		m.License = over.License
	}
	if over.Image != "" {
		m.Image = over.Image
	}
	if over.TagDefault != "" {
		m.TagDefault = over.TagDefault
	}
	if over.TagsAvailable != nil {
		m.TagsAvailable = over.TagsAvailable
	}
	if over.Visibility != "" {
		m.Visibility = over.Visibility
	}
	if over.Recipe != "" {
		m.Recipe = over.Recipe
	}
	if over.AMIs != nil {
		m.AMIs = over.AMIs
	}
	if over.BaseAMIs != nil {
		m.BaseAMIs = over.BaseAMIs
	}
	return m
}
