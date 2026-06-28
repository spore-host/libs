//go:build online

// Online catalog gate (libs#18): verifies every container image in the SHIPPED
// catalog is public and anonymously pullable. Network-dependent, so it's behind
// the `online` build tag and run deliberately in CI (`go test -tags online ./catalog/`),
// not on every `go test`.
package catalog

import "testing"

func TestEmbeddedCatalogImagesAreAnonymouslyPullable(t *testing.T) {
	errs := ResolvePublicImages(List())
	for _, e := range errs {
		t.Errorf("shipped catalog image unresolvable: %v", e)
	}
}
