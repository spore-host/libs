// Package docgen renders a cobra command tree into deterministic, timestamp-free
// markdown fragments for the spore.host docs site. It is the single source of the
// exhaustive command/flag reference: each CLI (spawn/truffle/lagotto) exposes a
// hidden `gen-docs` command that calls Generate, commits the output, and a CI
// drift gate fails if the committed output ever diverges from the code — so the
// reference cannot silently rot.
//
// Only the exhaustive tables are generated; hand-written prose (guides, overviews)
// lives elsewhere. Output honors cobra's Hidden (skipped) and Deprecated (flagged)
// so deprecations surface in docs automatically. It is intentionally NOT
// cobra/doc's man-page output: the format matches the site's flag-table style and
// carries no timestamps, so `git diff` is meaningful.
package docgen

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Options configures a Generate run.
type Options struct {
	// CLIName is the binary name used in headings/usage (e.g. "spawn"). Defaults
	// to root.Name() when empty.
	CLIName string
}

// Generate walks root and returns markdown fragments keyed by filename:
//   - "<top-level-command>.md" for each visible top-level command (recursing into
//     its subcommands), and
//   - "_global-flags.md" documenting the root's persistent flags once.
//
// cobra's auto-generated "completion" and "help" commands are skipped, as are any
// Hidden commands. The caller writes each fragment to disk (e.g. docs-gen/<key>).
func Generate(root *cobra.Command, opts Options) (map[string][]byte, error) {
	if root == nil {
		return nil, fmt.Errorf("docgen: nil root command")
	}
	cli := opts.CLIName
	if cli == "" {
		cli = root.Name()
	}

	out := make(map[string][]byte)

	// Global (persistent) flags fragment.
	if pf := root.PersistentFlags(); pf.HasAvailableFlags() {
		var b strings.Builder
		fmt.Fprintf(&b, "### Global flags\n\n")
		fmt.Fprintf(&b, "These apply to every `%s` command.\n\n", cli)
		writeFlagTable(&b, pf)
		out["_global-flags.md"] = []byte(b.String())
	}

	for _, c := range root.Commands() {
		if skip(c) {
			continue
		}
		var b strings.Builder
		writeCommand(&b, c, cli, 2)
		out[c.Name()+".md"] = []byte(b.String())
	}
	return out, nil
}

// skip reports whether a command is excluded from the reference. Hidden and the
// auto-generated helper commands are dropped; Deprecated commands are KEPT (and
// flagged) so the deprecation is visible in the docs.
func skip(c *cobra.Command) bool {
	if c.Hidden || !c.Runnable() && !c.HasAvailableSubCommands() {
		return true
	}
	switch c.Name() {
	case "completion", "help", "gen-docs":
		return true
	}
	return false
}

// writeCommand renders one command at the given heading level and recurses into
// its visible subcommands at level+1.
func writeCommand(b *strings.Builder, c *cobra.Command, cli string, level int) {
	fmt.Fprintf(b, "%s `%s %s`\n\n", strings.Repeat("#", level), cli, c.CommandPath()[len(cli)+1:])

	if c.Deprecated != "" {
		fmt.Fprintf(b, "> **Deprecated:** %s\n\n", escapeAnglesInline(c.Deprecated))
	}
	if c.Aliases != nil && len(c.Aliases) > 0 {
		fmt.Fprintf(b, "*Aliases: %s*\n\n", strings.Join(c.Aliases, ", "))
	}

	desc := c.Long
	if strings.TrimSpace(desc) == "" {
		desc = c.Short
	}
	if strings.TrimSpace(desc) != "" {
		fmt.Fprintf(b, "%s\n\n", escapeAnglesBlock(strings.TrimSpace(desc)))
	}

	fmt.Fprintf(b, "```\n%s\n```\n\n", c.UseLine())

	if ex := strings.TrimSpace(c.Example); ex != "" {
		fmt.Fprintf(b, "**Examples:**\n\n```sh\n%s\n```\n\n", ex)
	}

	// Local flags only (persistent/global flags are documented once in _global-flags.md).
	if lf := c.LocalFlags(); lf.HasAvailableFlags() {
		fmt.Fprintf(b, "**Flags:**\n\n")
		writeFlagTable(b, lf)
	}

	// Recurse into visible subcommands.
	for _, sub := range c.Commands() {
		if skip(sub) {
			continue
		}
		writeCommand(b, sub, cli, level+1)
	}
}

// writeFlagTable emits a deterministic markdown table of the flag set's available
// (non-hidden) flags, sorted by name for stable diffs.
func writeFlagTable(b *strings.Builder, fs *pflag.FlagSet) {
	type row struct{ name, short, typ, def, usage string }
	var rows []row
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		short := ""
		if f.Shorthand != "" {
			short = "`-" + f.Shorthand + "`"
		}
		usage := f.Usage
		if f.Deprecated != "" {
			usage = "**Deprecated** (" + f.Deprecated + "). " + usage
		}
		rows = append(rows, row{
			name:  "`--" + f.Name + "`",
			short: short,
			typ:   f.Value.Type(),
			def:   defaultCell(f),
			usage: escapeAnglesInline(escapePipes(usage)),
		})
	})
	if len(rows) == 0 {
		return
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	fmt.Fprintf(b, "| Flag | Short | Type | Default | Description |\n")
	fmt.Fprintf(b, "|------|-------|------|---------|-------------|\n")
	for _, r := range rows {
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n", r.name, r.short, r.typ, r.def, r.usage)
	}
	b.WriteString("\n")
}

// defaultCell formats a flag's default for the table; empty/false/0 defaults show
// as a blank cell to reduce noise.
func defaultCell(f *pflag.Flag) string {
	switch f.DefValue {
	case "", "false", "0", "[]", "map[]":
		return ""
	default:
		return "`" + escapePipes(f.DefValue) + "`"
	}
}

// escapePipes keeps table cells from breaking on literal pipes/newlines.
func escapePipes(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "|", "\\|")
}

// escapeAnglesInline HTML-escapes `<`/`>` in a single line of prose so VitePress
// (which runs markdown through Vue's template compiler) doesn't treat a
// placeholder like `<sweep-id>` as an unclosed HTML tag. Text already inside a
// backtick code span is left untouched — there the angles render literally and
// are not parsed as HTML.
func escapeAnglesInline(s string) string {
	var b strings.Builder
	inCode := false
	for _, r := range s {
		switch r {
		case '`':
			inCode = !inCode
			b.WriteRune(r)
		case '<':
			if inCode {
				b.WriteRune(r)
			} else {
				b.WriteString("&lt;")
			}
		case '>':
			if inCode {
				b.WriteRune(r)
			} else {
				b.WriteString("&gt;")
			}
		default:
			b.WriteRune(r)
		}
	}
	return s2OrEscaped(s, b.String(), inCode)
}

// s2OrEscaped guards against an unbalanced backtick run in the source (which would
// leave inCode true at end-of-string and mean we skipped escaping a real angle):
// in that pathological case, fall back to escaping every angle so the fragment
// still compiles. Balanced input returns the properly-escaped string.
func s2OrEscaped(orig, escaped string, unbalanced bool) string {
	if !unbalanced {
		return escaped
	}
	e := strings.ReplaceAll(orig, "<", "&lt;")
	return strings.ReplaceAll(e, ">", "&gt;")
}

// escapeAnglesBlock applies escapeAnglesInline line-by-line across a multi-line
// block, skipping fenced code blocks (``` … ```) and indented (4-space/tab) code
// lines entirely — angles there are literal code, not markup.
func escapeAnglesBlock(s string) string {
	lines := strings.Split(s, "\n")
	inFence := false
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if strings.HasPrefix(ln, "    ") || strings.HasPrefix(ln, "\t") {
			continue
		}
		lines[i] = escapeAnglesInline(ln)
	}
	return strings.Join(lines, "\n")
}
