// Package hygiene holds tests that assert on repo wiring rather than on code.
//
// It exists because wiring is what rots: a CI step is a one-line deletion whose
// absence is completely silent — nothing fails, the tree simply starts drifting
// again, and nobody notices until an unrelated PR picks up a reformatting diff.
// That is exactly how three files came to sit unformatted on main (libs#30).
// There are no non-test files here by design; the assertions are about the repo.
package hygiene

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestCIGatesFormatting checks that the formatting gate is still wired into CI,
// and that it REPORTS drift rather than fixing it.
//
// The second half is the subtle one. `gofmt -w` rewrites files and exits 0, so a
// "gate" built on it can only ever report success — green on a dirty tree
// forever, indistinguishable from having no gate at all. Only `gofmt -l`/`-d`
// can fail. This repo has no Makefile, so the gate is inline in the workflow.
func TestCIGatesFormatting(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	step, ok := workflowStep(string(data), "Format gate")
	if !ok {
		t.Fatal("the CI workflow has no 'Format gate' step; without it nothing " +
			"prevents unformatted code from reaching main (libs#30)")
	}
	if !strings.Contains(step, "gofmt -l") {
		t.Error("the Format gate does not run 'gofmt -l'; it must LIST offenders to be able to fail")
	}
	// Commands only, not message text: the step's error message tells the reader
	// to "run 'gofmt -w' on them", which is advice, not an invocation.
	if strings.Contains(unquote(step), "gofmt -w") {
		t.Error("the Format gate runs 'gofmt -w': that rewrites files and always exits 0, " +
			"so it reports success on a dirty tree. A gate must report, not fix.")
	}
	if !strings.Contains(step, "exit 1") {
		t.Error("the Format gate never exits non-zero, so CI can't fail on drift")
	}
}

// workflowStep returns the YAML block for the step named name: from its "- name:"
// line up to the next line at the same indentation starting a new list item.
// Scoping to one step matters — otherwise an assertion could be satisfied by an
// unrelated step elsewhere in the file.
func workflowStep(yaml, name string) (string, bool) {
	lines := strings.Split(yaml, "\n")
	start := -1
	var indent string
	for i, line := range lines {
		if strings.HasSuffix(strings.TrimSpace(line), "name: "+name) &&
			strings.HasPrefix(strings.TrimSpace(line), "- ") {
			start = i
			indent = line[:strings.Index(line, "- ")]
			break
		}
	}
	if start < 0 {
		return "", false
	}
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], indent+"- ") {
			return strings.Join(lines[start:i], "\n"), true
		}
	}
	return strings.Join(lines[start:], "\n"), true
}

// unquote strips single- and double-quoted spans, leaving roughly the commands.
// Crude, but the question it answers is narrow: does the step RUN something, or
// merely mention it in a message?
func unquote(s string) string {
	var b strings.Builder
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// TestActionsArePinnedToSHAs: every `uses:` in a workflow must name a full
// 40-hex commit SHA, not a tag.
//
// A tag is mutable. `actions/checkout@v6` means "whatever v6 points at when the
// job runs", so the code executing in CI — with a checkout of this repo and a
// token — can change without a commit here. That is a supply-chain hole, and it
// is not hypothetical for this repo: while pinning these, `actions/checkout@v6`
// had already moved from the v6.0.3-era commit the sibling repos pinned to a
// later one, silently, exactly as designed.
//
// The trailing `# vX.Y.Z` comment is required too. A bare SHA is unreadable, and
// the version is what makes a bump reviewable — without it nobody can tell
// whether a pin is current or two years stale.
func TestActionsArePinnedToSHAs(t *testing.T) {
	entries, err := os.ReadDir("../../.github/workflows")
	if err != nil {
		t.Fatalf("read workflows dir: %v", err)
	}
	// A local `uses: ./.github/...` is a path, not a registry ref — nothing to pin.
	pinned := regexp.MustCompile(`^[^@\s]+@[0-9a-f]{40}\s+#\s*v?\d`)
	found := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") && !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join("../../.github/workflows", e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			trimmed = strings.TrimPrefix(trimmed, "- ")
			if !strings.HasPrefix(trimmed, "uses:") {
				continue
			}
			ref := strings.TrimSpace(strings.TrimPrefix(trimmed, "uses:"))
			if strings.HasPrefix(ref, "./") {
				continue
			}
			found++
			if !pinned.MatchString(ref) {
				t.Errorf("%s:%d: %q is not pinned to a full commit SHA with a version comment.\n"+
					"A tag is mutable, so the code CI runs can change without a commit here. Use:\n"+
					"    uses: owner/action@<40-hex-sha> # vX.Y.Z",
					e.Name(), i+1, ref)
			}
		}
	}
	if found == 0 {
		t.Error("no `uses:` lines found in .github/workflows — this test is asserting nothing; " +
			"check the parser against the current workflow layout")
	}
}
