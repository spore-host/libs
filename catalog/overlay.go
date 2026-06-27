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
// whose Name matches a base entry REPLACES it wholesale (the overlay owns that
// app's definition + image binding); an overlay entry with a new Name is
// appended. Pure — unit-tested without the filesystem. Order is not significant
// (the caller sorts).
func mergeApps(base, over []AppEntry) []AppEntry {
	idx := make(map[string]int, len(base)) // lowercased name → index in result
	out := make([]AppEntry, len(base))
	copy(out, base)
	for i := range out {
		idx[strings.ToLower(out[i].Name)] = i
	}
	for _, e := range over {
		if i, ok := idx[strings.ToLower(e.Name)]; ok {
			out[i] = e // overlay wins
		} else {
			idx[strings.ToLower(e.Name)] = len(out)
			out = append(out, e)
		}
	}
	return out
}
