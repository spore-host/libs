package docgen

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// buildTree constructs a representative command tree: a root with persistent
// flags, a normal command with local flags + example, a deprecated command, a
// hidden command (must be skipped), and a nested subcommand group.
func buildTree() *cobra.Command {
	root := &cobra.Command{Use: "demo", Short: "demo root"}
	root.PersistentFlags().String("profile", "", "AWS profile")
	root.PersistentFlags().BoolP("verbose", "v", false, "verbose output")

	launch := &cobra.Command{
		Use:   "launch <name>",
		Short: "launch a thing",
		// Long carries a bare <placeholder> in prose and a fenced code block whose
		// <angles> must stay literal.
		Long:    "Launch a thing named <name>.\n\n    demo launch <name> --count 3",
		Example: "demo launch web --count 3",
		Run:     func(*cobra.Command, []string) {},
	}
	launch.Flags().Int("count", 1, "how many")
	launch.Flags().String("ttl", "1h", "time to live")
	// A flag whose usage carries a bare <placeholder> and a `<code>` span — the
	// former must be HTML-escaped (VitePress runs markdown through Vue), the latter
	// must be left literal inside the code span.
	launch.Flags().String("dest", "", "path like <bucket>/key (default: `s3://b/<id>`)")
	// A flag whose usage carries a Vue mustache — must be neutralized so VitePress
	// doesn't evaluate it as an interpolation expression and crash rendering.
	launch.Flags().String("template", "", "substitutes {{ config.X }} at runtime")
	root.AddCommand(launch)

	old := &cobra.Command{Use: "old", Short: "old cmd", Deprecated: "use launch instead", Run: func(*cobra.Command, []string) {}}
	root.AddCommand(old)

	secret := &cobra.Command{Use: "secret", Short: "hidden", Hidden: true, Run: func(*cobra.Command, []string) {}}
	root.AddCommand(secret)

	notify := &cobra.Command{Use: "notify", Short: "notifications"}
	ws := &cobra.Command{Use: "workspace", Short: "workspaces"}
	add := &cobra.Command{Use: "add", Short: "add a workspace", Run: func(*cobra.Command, []string) {}}
	ws.AddCommand(add)
	notify.AddCommand(ws)
	root.AddCommand(notify)

	return root
}

func TestGenerate_FilesAndContent(t *testing.T) {
	out, err := Generate(buildTree(), Options{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// One fragment per visible top-level command + the global-flags file.
	for _, want := range []string{"_global-flags.md", "launch.md", "old.md", "notify.md"} {
		if _, ok := out[want]; !ok {
			t.Errorf("missing fragment %q", want)
		}
	}
	// Hidden command is skipped.
	if _, ok := out["secret.md"]; ok {
		t.Error("hidden command was emitted")
	}

	launch := string(out["launch.md"])
	if !strings.Contains(launch, "## `demo launch`") {
		t.Errorf("launch heading missing:\n%s", launch)
	}
	if !strings.Contains(launch, "Launch a thing named &lt;name&gt;.") {
		t.Errorf("launch Long prose <placeholder> not HTML-escaped:\n%s", launch)
	}
	// Bare angle in an indented code block within Long stays literal.
	if !strings.Contains(launch, "demo launch <name> --count 3") {
		t.Errorf("angle inside indented code block was escaped:\n%s", launch)
	}
	if !strings.Contains(launch, "demo launch web --count 3") {
		t.Error("launch example missing")
	}
	for _, f := range []string{"`--count`", "`--ttl`", "`--dest`"} {
		if !strings.Contains(launch, f) {
			t.Errorf("launch flag %s missing from table", f)
		}
	}
	// The --dest usage: bare <bucket> escaped, `<id>` inside a code span left literal.
	if !strings.Contains(launch, "path like &lt;bucket&gt;/key") {
		t.Errorf("bare <placeholder> in flag usage not escaped:\n%s", launch)
	}
	if !strings.Contains(launch, "`s3://b/<id>`") {
		t.Errorf("angle inside a flag-usage code span was escaped:\n%s", launch)
	}
	// Vue mustache neutralized so it isn't evaluated as an interpolation.
	if strings.Contains(launch, "{{ config.X }}") {
		t.Errorf("Vue mustache left un-escaped in flag usage:\n%s", launch)
	}
	if !strings.Contains(launch, "&#123;&#123; config.X &#125;&#125;") {
		t.Errorf("Vue mustache not escaped to HTML entities:\n%s", launch)
	}
	// Local flag table must NOT include the persistent --profile (documented once).
	if strings.Contains(launch, "`--profile`") {
		t.Error("persistent flag leaked into a command's local flag table")
	}

	// Deprecation surfaces automatically.
	if !strings.Contains(string(out["old.md"]), "Deprecated") {
		t.Error("deprecated command not flagged")
	}

	// Nested subcommand is rendered under its parent.
	notify := string(out["notify.md"])
	if !strings.Contains(notify, "`demo notify workspace add`") {
		t.Errorf("nested subcommand heading missing:\n%s", notify)
	}

	// Global flags file has the persistent flags.
	gf := string(out["_global-flags.md"])
	if !strings.Contains(gf, "`--profile`") || !strings.Contains(gf, "`--verbose`") {
		t.Errorf("global flags fragment incomplete:\n%s", gf)
	}
}

// TestGenerate_Deterministic: same tree → byte-identical output (no timestamps,
// stable flag sort) so the drift gate's git-diff is meaningful.
func TestGenerate_Deterministic(t *testing.T) {
	a, _ := Generate(buildTree(), Options{})
	b, _ := Generate(buildTree(), Options{})
	if len(a) != len(b) {
		t.Fatalf("fragment count differs: %d vs %d", len(a), len(b))
	}
	for k, va := range a {
		if string(va) != string(b[k]) {
			t.Errorf("fragment %q not deterministic", k)
		}
	}
}
