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

	"gopkg.in/yaml.v3"
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

// TestDependabotCoversEveryAction is the other half of pinning to SHAs.
//
// A pin closes the mutable-tag hole but opens a staleness one: a SHA never moves,
// including past a security fix, and unlike `@v6` nothing updates it for you.
// So pinning is only safe if something bumps the pins — here, Dependabot. Without
// it the workflow slowly freezes on old actions and no signal ever says so, since
// this repo has no security workflow to flag it either.
//
// The check that matters is coverage: an ecosystem entry that doesn't match an
// action leaves that action unmanaged, silently. Grouping is by "*" rather than
// the umbrella's "actions/*" precisely because codecov/codecov-action wouldn't
// match the latter.
func TestDependabotCoversEveryAction(t *testing.T) {
	data, err := os.ReadFile("../../.github/dependabot.yml")
	if err != nil {
		t.Fatalf("read dependabot.yml: %v (CI's actions are pinned to SHAs; without "+
			"Dependabot nothing ever bumps them)", err)
	}
	var cfg struct {
		Version int `yaml:"version"`
		Updates []struct {
			Ecosystem string `yaml:"package-ecosystem"`
			Directory string `yaml:"directory"`
			Groups    map[string]struct {
				Patterns []string `yaml:"patterns"`
			} `yaml:"groups"`
		} `yaml:"updates"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("dependabot.yml is not valid YAML: %v", err)
	}
	if cfg.Version != 2 {
		t.Errorf("dependabot.yml version = %d, want 2 (v1 is unsupported)", cfg.Version)
	}

	var patterns []string
	ok := false
	for _, u := range cfg.Updates {
		if u.Ecosystem != "github-actions" {
			continue
		}
		ok = true
		if u.Directory != "/" {
			t.Errorf("the github-actions entry watches %q; workflows live in /.github/workflows, "+
				"which Dependabot finds via directory \"/\"", u.Directory)
		}
		for _, g := range u.Groups {
			patterns = append(patterns, g.Patterns...)
		}
	}
	if !ok {
		t.Fatal("dependabot.yml has no `github-actions` entry, so the SHA-pinned " +
			"actions in .github/workflows are never bumped")
	}

	// Every action in every workflow must be matched by some group pattern.
	for _, action := range workflowActions(t) {
		matched := false
		for _, p := range patterns {
			if globMatch(p, action) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("%s is not matched by any Dependabot group pattern %v, so it would "+
				"open its own PR outside the group (or be missed). Widen the pattern.",
				action, patterns)
		}
	}
}

// workflowActions returns the deduplicated owner/name of every registry action
// used by the workflows (local `./...` refs excluded — nothing to update).
func workflowActions(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("../../.github/workflows")
	if err != nil {
		t.Fatalf("read workflows dir: %v", err)
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") && !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join("../../.github/workflows", e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimPrefix(strings.TrimSpace(line), "- ")
			if !strings.HasPrefix(trimmed, "uses:") {
				continue
			}
			ref := strings.TrimSpace(strings.TrimPrefix(trimmed, "uses:"))
			if strings.HasPrefix(ref, "./") {
				continue
			}
			name, _, _ := strings.Cut(ref, "@")
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("found no actions in .github/workflows — this test would assert nothing; " +
			"check the parser against the current workflow layout")
	}
	return out
}

// globMatch implements the only wildcard Dependabot patterns use: `*`, matching
// any run of characters (including `/`, so `*` alone matches everything).
func globMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(s, p)
		if i < 0 {
			return false
		}
		s = s[i+len(p):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}
