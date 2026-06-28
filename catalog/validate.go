package catalog

import (
	"fmt"
	"sort"
)

// Validate checks the embedded catalog for the structural defects that caused
// the #389 outage, with no AWS calls — so it runs as a fast CI gate (and at
// startup if a caller wants). It returns one error per problem found, or nil.
//
// What it enforces (offline):
//   - every app is launchable: container Image or a legacy LaunchCommand;
//   - no app reintroduces the deprecated per-app AMI table (the #389 data);
//   - a container app has a non-empty TagDefault, TagDefault is within
//     TagsAvailable (when that list is set), and at least one BaseAMIs region;
//   - no two apps share a base AMI ID for the *same* region only by accident —
//     duplication across apps is allowed for BaseAMIs (the base is shared by
//     design), but a container app's Image must be unique per app.
//
// What it does NOT check (needs AWS creds → a separate authenticated job):
//   - that each Image:tag actually resolves in ECR;
//   - that each BaseAMIs entry is launch-visible from the launch account.
func Validate() []error {
	apps := List()
	errs := validateApps(apps)
	// The shipped/global catalog must contain only PUBLIC images (#392): a private
	// image here is unlaunchable for everyone but its owner, so it has no place in
	// the artifact shipped to all consumers. Private images belong in a user's
	// local overlay. (This is the offline half; online resolvability is a separate
	// authenticated CI gate, libs#18.) validateApps stays overlay-safe — it does
	// NOT enforce this, since overlays legitimately carry private images.
	for _, app := range apps {
		if app.Containerized() && app.ImageVisibility() != VisibilityPublic {
			errs = append(errs, fmt.Errorf("%s: image %q is %s — the shipped catalog must be public; put private images in a local overlay (#392)",
				app.Name, app.Image, app.ImageVisibility()))
		}
	}
	return errs
}

// validateApps is the pure core of Validate, taking the app list explicitly so
// synthetic bad entries can be unit-tested without touching the embedded catalog.
func validateApps(apps []AppEntry) []error {
	var errs []error
	images := map[string]string{} // image → first app that used it

	for _, app := range apps {
		if app.Image == "" && app.LaunchCommand == "" && app.Recipe == "" {
			errs = append(errs, fmt.Errorf("%s: not usable (no image, no launch_command, no recipe)", app.Name))
		}
		if len(app.AMIs) != 0 {
			errs = append(errs, fmt.Errorf("%s: uses the deprecated per-app amis table (%v) — use image + base_amis (#389)", app.Name, sortedKeys(app.AMIs)))
		}
		if !app.Containerized() {
			continue
		}
		if app.TagDefault == "" {
			errs = append(errs, fmt.Errorf("%s: container app has no tag_default", app.Name))
		}
		if len(app.TagsAvailable) > 0 {
			found := false
			for _, t := range app.TagsAvailable {
				if t == app.TagDefault {
					found = true
					break
				}
			}
			if !found {
				errs = append(errs, fmt.Errorf("%s: tag_default %q is not in tags_available %v", app.Name, app.TagDefault, app.TagsAvailable))
			}
		}
		if len(app.BaseAMIs) == 0 || allEmpty(app.BaseAMIs) {
			errs = append(errs, fmt.Errorf("%s: container app has no base_amis", app.Name))
		}
		if prev, ok := images[app.Image]; ok {
			errs = append(errs, fmt.Errorf("%s: image %q is also used by %q — each app needs its own image", app.Name, app.Image, prev))
		} else {
			images[app.Image] = app.Name
		}
	}
	return errs
}

func allEmpty(m map[string]string) bool {
	for _, v := range m {
		if v != "" {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
