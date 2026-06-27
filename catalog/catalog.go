// Package catalog provides the spore.host application catalog — a registry of
// streamable research applications with their hardware requirements and container
// image bindings. Both truffle (hardware discovery) and spawn (instance launch)
// import this package to resolve application names to EC2 configuration.
//
// The embedded catalog.yaml is the global, public baseline shipped to every
// consumer. A user can layer a local overlay (see overlay.go) to add their own
// apps or rebind an app's image to one they host — the only place private images
// belong (BYO-image model, #392).
package catalog

import (
	_ "embed"
	"fmt"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed catalog.yaml
var catalogData []byte

// AppEntry describes a streamable application in the spore.host catalog.
type AppEntry struct {
	// Name is the canonical lowercase identifier (e.g. "paraview", "igv").
	Name string `yaml:"name"`
	// Description is a short human-readable description.
	Description string `yaml:"description"`
	// InstanceFamilies lists recommended EC2 instance families in preference order.
	InstanceFamilies []string `yaml:"instance_families"`
	// HighVRAMFamilies lists instance families for large-dataset / high-VRAM workloads.
	HighVRAMFamilies []string `yaml:"high_vram_families"`
	// MinVCPUs is the minimum number of vCPUs required.
	MinVCPUs int `yaml:"min_vcpus"`
	// MinMemoryGiB is the minimum memory in GiB required.
	MinMemoryGiB int `yaml:"min_memory_gib"`
	// GPU indicates whether a GPU is required.
	GPU bool `yaml:"gpu"`
	// MinVRAMGiB is the minimum GPU VRAM in GiB (only relevant when GPU is true).
	MinVRAMGiB int `yaml:"min_vram_gib"`
	// DCVEnabled indicates the app is configured for NICE DCV application streaming.
	DCVEnabled bool `yaml:"dcv"`
	// IdleTimeoutDefault is the recommended idle timeout (e.g. "20m").
	IdleTimeoutDefault string `yaml:"idle_timeout_default"`
	// LaunchCommand is the full path to the application binary on the AMI.
	LaunchCommand string `yaml:"launch_command"`
	// Aliases are alternative names that resolve to this entry (e.g. "pv" → "paraview").
	Aliases []string `yaml:"aliases"`
	// License describes the licensing model: "open-source", "commercial", "needs-conversation".
	License string `yaml:"license"`

	// Container-based catalog (#290). The app ships as a Docker image pulled at
	// launch onto a single shared DCV base AMI, instead of a baked per-app AMI —
	// which removes the per-app-per-region AMI table that drifted into dangling
	// and duplicated IDs (#389).

	// Image is the container image (without tag) the app runs from, e.g.
	// "public.ecr.aws/spore-host/paraview". Empty for a not-yet-containerized app.
	Image string `yaml:"image"`
	// TagDefault is the image tag launched when --app-version is not given (e.g. "5.13.2").
	TagDefault string `yaml:"tag_default"`
	// TagsAvailable lists the image tags a user may select via --app-version.
	// Always includes TagDefault. Used to validate --app-version before launch.
	TagsAvailable []string `yaml:"tags_available"`
	// Visibility is "public" (anonymously pullable by any account) or "private"
	// (needs registry auth + a cross-account grant). When empty it is inferred
	// from Image (see ImageVisibility). spore.host ships only public images in the
	// global catalog; private images come from a user's local overlay and are
	// listed/launchable only for accounts that can pull them (BYO model, #392).
	Visibility string `yaml:"visibility"`

	// AMIs maps AWS region to a per-app baked AMI ID. DEPRECATED (#290): superseded
	// by the shared base AMI (BaseAMIs) + Image. Retained one release so a stale
	// consumer doesn't break; new entries must not set it. Every value here was
	// found dangling/unshared from the launch account (#389) — do not trust it.
	AMIs map[string]string `yaml:"amis"`
	// BaseAMIs maps AWS region to the shared spore-dcv-base AMI ID (DCV + NVIDIA +
	// Docker + NVIDIA Container Toolkit + spored). One image per region serves all
	// container apps. Must be shared/visible to the launch account (#389 root cause).
	BaseAMIs map[string]string `yaml:"base_amis"`
}

// ResolveTag returns the image tag to launch for the requested version: the
// requested tag if it is allowed, TagDefault when requested is empty, or an
// error naming the available tags. Pure, so the CLI validates --app-version
// without any AWS calls (#290).
func (e *AppEntry) ResolveTag(requested string) (string, error) {
	if requested == "" {
		if e.TagDefault == "" {
			return "", fmt.Errorf("app %q has no default image tag", e.Name)
		}
		return e.TagDefault, nil
	}
	for _, t := range e.TagsAvailable {
		if t == requested {
			return requested, nil
		}
	}
	// TagDefault is always implicitly available even if omitted from the list.
	if requested == e.TagDefault {
		return requested, nil
	}
	avail := e.TagsAvailable
	if len(avail) == 0 && e.TagDefault != "" {
		avail = []string{e.TagDefault}
	}
	return "", fmt.Errorf("version %q not available for %s (available: %s)", requested, e.Name, strings.Join(avail, ", "))
}

// Containerized reports whether the app launches from a container image (#290)
// rather than a deprecated baked per-app AMI.
func (e *AppEntry) Containerized() bool { return e.Image != "" }

// Image visibility values (the BYO-image model, #392).
const (
	VisibilityPublic  = "public"  // anonymously pullable by any account
	VisibilityPrivate = "private" // needs registry auth + a cross-account grant
)

// ImageVisibility returns the effective visibility of the app's image: the
// explicit Visibility if set, otherwise inferred from the registry host —
// public.ecr.aws/* is public, an *.dkr.ecr.<region>.amazonaws.com/* (private
// ECR) registry is private. Anything else (other registries, no image) is
// treated as public, the safe default for the launchability filter: a public
// guess that turns out unpullable is caught at launch, whereas a wrong "private"
// guess would hide a launchable app. Used by the per-account list/launch filter
// (#392) — public images list everywhere; private images list only for accounts
// that can pull them.
func (e *AppEntry) ImageVisibility() string {
	switch e.Visibility {
	case VisibilityPublic, VisibilityPrivate:
		return e.Visibility
	}
	if isPrivateRegistry(e.Image) {
		return VisibilityPrivate
	}
	return VisibilityPublic
}

// isPrivateRegistry reports whether an image ref names a private registry that
// requires authentication. Currently recognizes private ECR
// (<account>.dkr.ecr.<region>.amazonaws.com); public.ecr.aws and other hosts are
// treated as public.
func isPrivateRegistry(image string) bool {
	if image == "" {
		return false
	}
	host := image
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	return strings.Contains(host, ".dkr.ecr.") && strings.HasSuffix(host, ".amazonaws.com")
}

type catalogFile struct {
	Apps []AppEntry `yaml:"apps"`
}

var (
	mu         sync.Mutex
	loaded     bool
	byName     map[string]*AppEntry // canonical name → entry
	byAlias    map[string]*AppEntry // alias → entry (includes canonical names)
	allSorted  []AppEntry
	overlayErr error // non-fatal error from loading the user overlay, if any
)

// load builds the catalog on first use (lazily). Subsequent calls are no-ops
// until Reload forces a rebuild.
func load() {
	mu.Lock()
	defer mu.Unlock()
	if !loaded {
		build()
		loaded = true
	}
}

// build parses the embedded catalog, merges the user overlay (if any), and
// (re)builds the lookup maps. Caller must hold mu.
func build() {
	var f catalogFile
	if err := yaml.Unmarshal(catalogData, &f); err != nil {
		panic("catalog: failed to parse catalog.yaml: " + err.Error())
	}

	apps := f.Apps
	// Merge the user overlay on top of the embedded catalog (BYO model, #392).
	// A bad overlay is non-fatal: record the error and fall back to embedded-only
	// so the tool still works; callers can surface it via LoadError.
	overlayErr = nil
	if ov, err := loadOverlayApps(); err != nil {
		overlayErr = err
	} else if len(ov) > 0 {
		apps = mergeApps(apps, ov)
	}

	// Sort first, then build maps so pointer addresses are stable.
	allSorted = make([]AppEntry, len(apps))
	copy(allSorted, apps)
	sort.Slice(allSorted, func(i, j int) bool {
		return allSorted[i].Name < allSorted[j].Name
	})

	byName = make(map[string]*AppEntry, len(allSorted))
	byAlias = make(map[string]*AppEntry)
	for i := range allSorted {
		e := &allSorted[i]
		byName[e.Name] = e
		byAlias[e.Name] = e
		for _, a := range e.Aliases {
			byAlias[strings.ToLower(a)] = e
		}
	}
}

// Reload rebuilds the catalog, re-reading the user overlay. Call it after
// SetOverlayPath (e.g. once a --catalog flag is parsed). Not safe to call
// concurrently with in-flight Lookup/List; callers should Reload during startup
// before serving.
func Reload() {
	mu.Lock()
	defer mu.Unlock()
	build()
	loaded = true
}

// LoadError returns any non-fatal error encountered loading the user overlay on
// the last build (nil if the overlay was absent or valid). The embedded catalog
// is always available regardless.
func LoadError() error {
	load()
	mu.Lock()
	defer mu.Unlock()
	return overlayErr
}

// Lookup returns the AppEntry for name (canonical name or alias), case-insensitive.
// Returns nil, false if the name is not in the catalog.
func Lookup(name string) (*AppEntry, bool) {
	load()
	key := strings.ToLower(strings.TrimSpace(name))
	e, ok := byAlias[key]
	return e, ok
}

// List returns all catalog entries sorted alphabetically by name.
func List() []AppEntry {
	load()
	return allSorted
}
