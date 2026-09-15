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
//   - a container app has a non-empty TagDefault and TagDefault is within
//     TagsAvailable (when that list is set);
//   - a container app's Image is unique per app.
//
// It does NOT require BaseAMIs: the base AMI is resolved at launch from the
// AWS-maintained GPU DLAMI via SSM (spore-host#286/#389), so BaseAMIs is an
// optional pin, not a requirement. Owning a per-region base-AMI table was the
// source of #389 (dangling / unshared / duplicated IDs); there is nothing to
// own now.
//
// What it does NOT check (needs AWS creds → a separate authenticated job):
//   - that each Image:tag actually resolves in ECR;
//   - that a pinned BaseAMIs entry is launch-visible from the launch account.
func Validate() []error {
	apps := List()
	errs := validateApps(apps)
	// The shipped/global catalog must contain only PUBLIC images (#392): a private
	// image here is unlaunchable for everyone but its owner, so it has no place in
	// the artifact shipped to all consumers. Private images belong in a user's
	// local overlay — so this check runs against the EMBEDDED catalog only, not
	// List()'s overlay-merged result. Checking the merged list made Validate() (and
	// TestCatalogValid, which spawn's CI runs too) fail on any machine with a local
	// ~/.spawn/catalog.yaml binding a private image, exactly the case the overlay
	// exists for — the shipped catalog was never actually invalid, only the local
	// developer machine running the check was. (This is the offline half; online
	// resolvability is a separate authenticated CI gate, libs#18.) validateApps
	// stays overlay-safe — it does NOT enforce this, since overlays legitimately
	// carry private images.
	for _, app := range embeddedApps() {
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
		// A desktop kind legitimately has no image/launch_command/recipe — it runs
		// a desktop environment installed at boot, not a specific app (#591).
		if app.Kind() != KindDesktop && app.Image == "" && app.LaunchCommand == "" && app.Recipe == "" {
			errs = append(errs, fmt.Errorf("%s: not usable (no image, no launch_command, no recipe)", app.Name))
		}
		if len(app.AMIs) != 0 {
			errs = append(errs, fmt.Errorf("%s: uses the deprecated per-app amis table (%v) — use image + base_amis (#389)", app.Name, sortedKeys(app.AMIs)))
		}
		// Launch kind must be recognized (#590/#591); empty is fine (→ application).
		switch app.KindRaw {
		case "", KindApplication, KindDesktop, KindWeb:
		default:
			errs = append(errs, fmt.Errorf("%s: unknown kind %q (want %s, %s, or %s)", app.Name, app.KindRaw, KindApplication, KindDesktop, KindWeb))
		}
		// A web app serves its own UI on a port, so it needs Port and something to
		// run (image or launch_command). DCV kinds ignore Port.
		if app.Kind() == KindWeb {
			if app.Port <= 0 {
				errs = append(errs, fmt.Errorf("%s: web app has no port", app.Name))
			}
			if app.Image == "" && app.LaunchCommand == "" {
				errs = append(errs, fmt.Errorf("%s: web app has no image or launch_command to run", app.Name))
			}
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
		// BaseAMIs is intentionally NOT required: an unset/empty base_amis means
		// "resolve the AWS DLAMI base via SSM at launch" (spore-host#286/#389),
		// which is the default and recommended path. A populated base_amis is an
		// optional per-region pin for advanced use (e.g. a custom pre-baked image).
		if prev, ok := images[app.Image]; ok {
			errs = append(errs, fmt.Errorf("%s: image %q is also used by %q — each app needs its own image", app.Name, app.Image, prev))
		} else {
			images[app.Image] = app.Name
		}
	}
	return errs
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
