package i18n

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// truffleSearchInvocation matches a line that runs `truffle search`, the
// command truffle deprecated in favour of `truffle find`. Anchored on the
// invocation (start of line, after indentation) so prose mentioning the
// command, such as a deprecation note, is not a hit.
var truffleSearchInvocation = regexp.MustCompile(`(?m)^\s*truffle search\b`)

// TestHelpExamplesDoNotUseDeprecatedTruffleSearch: help text shows
// `truffle find`, not the deprecated `truffle search` (truffle#255). The root
// `truffle --help` examples and spawn's "from truffle" pipes all used it.
// Only truffle.search.* keys, the deprecated command's own help, may still
// invoke it. Languages and keys are discovered from the embedded catalogs,
// not listed here.
func TestHelpExamplesDoNotUseDeprecatedTruffleSearch(t *testing.T) {
	files, err := filepath.Glob("active.*.toml")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no active.*.toml catalogs found; the check would pass vacuously")
	}
	checked := 0
	for _, f := range files {
		lang := strings.TrimSuffix(strings.TrimPrefix(f, "active."), ".toml")
		entries, err := loadTranslations(lang)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if len(entries) == 0 {
			t.Fatalf("%s: no entries", f)
		}
		for key, e := range entries {
			checked++
			if strings.HasPrefix(key, "truffle.search.") {
				continue
			}
			for _, text := range []string{e.Other, e.One, e.Zero, e.Many} {
				if loc := truffleSearchInvocation.FindStringIndex(text); loc != nil {
					t.Errorf("%s: %s runs the deprecated `truffle search`; use `truffle find` (truffle#255): %q",
						f, key, strings.TrimSpace(text[loc[0]:min(loc[1]+30, len(text))]))
				}
			}
		}
	}
	t.Logf("checked %d entries in %d catalogs", checked, len(files))
}
