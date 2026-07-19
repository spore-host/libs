# Changelog

All notable changes to **libs** (shared Go libraries for the spore.host tools)
are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.43.2] - 2026-07-19

### Fixed
- **`docgen` now also neutralizes `{{ … }}` Vue mustaches** in prose and flag
  descriptions. VitePress evaluates `{{ config.X }}` as an interpolation
  expression and crashes page rendering; docgen now escapes the braces to HTML
  entities (outside code spans), completing the v0.43.1 angle-bracket fix.

## [0.43.1] - 2026-07-19

### Fixed
- **`docgen` now HTML-escapes `<`/`>` in prose and flag descriptions** so bare
  `<placeholder>` tokens (e.g. `<sweep-id>`, `shell: <cmd>`) don't break the
  VitePress build, which runs markdown through Vue's template compiler and reads
  `<foo>` as an unclosed HTML tag. Angles inside backtick code spans and fenced/
  indented code blocks are left literal, where they render correctly and aren't
  parsed as markup.

## [0.43.0] - 2026-07-19

### Added
- **New `docgen` package** — renders a cobra command tree into deterministic,
  timestamp-free markdown fragments (per-command flag tables + a global-flags
  file), honoring `Hidden` (skipped) and `Deprecated` (flagged). It's the shared
  engine behind each CLI's hidden `gen-docs` command and the docs drift gate, so
  the exhaustive command/flag reference is generated from code and can't silently
  go stale. Adds `github.com/spf13/cobra` as a direct dependency.

## [0.42.0] - 2026-07-17

### Added
- **New `sporeconfig` package** — the shared configuration base for the
  spore.host suite (spawn, truffle, lagotto, spore-host-mcp). `Resolve(Flags)`
  returns the common AWS `Profile`/`Region`/`Account` and default `Output` using
  a single precedence order — **flag > env > file > default** — so every tool
  resolves them the same way instead of each reinventing it. Reads `SPORE_*`
  (and `AWS_PROFILE`/`AWS_REGION`/`AWS_DEFAULT_REGION` as fallbacks) and the
  `[spore]` table of `~/.config/spore/config.toml` (XDG-aware via
  `ConfigDir`/`ConfigPath`). Deliberately **SDK-free**: it resolves strings only;
  each tool turns them into an `aws.Config` itself, and an unset value means
  "use the ambient AWS chain" so an unconfigured suite behaves exactly as before.
  A missing config file is not an error (opt-in); a malformed one is reported but
  the flag/env/default layers still resolve.

## [0.41.1] - 2026-06-28

### Fixed
- **catalog: overlay rebind now field-merges instead of replacing** (spore-host#392).
  An overlay entry that rebinds an existing app (e.g. supplying just an `image`)
  previously REPLACED the whole entry, blanking the app's description, GPU,
  families, etc. It now merges field-by-field: non-zero overlay fields override,
  unset fields inherit from the built-in definition. New apps in the overlay are
  still added as-is.

## [0.41.0] - 2026-06-28

### Added
- **catalog: `recipe` field — public recipe, private cake** (spore-host#392). An
  app can ship a public build-instructions pointer (`recipe:`, e.g.
  `infra/amis/containers/paraview`) without a bound image. Such a "recipe-only"
  entry (`AppEntry.RecipeOnly()`) is a buildable definition — anyone can bake the
  image and bind it via a local overlay or `--image`. `Validate()` accepts
  recipe-only entries as usable.

### Changed
- **catalog: paraview and chimerax are now recipe-only** (spore-host#392).
  Their image bindings were removed from the shipped catalog (they pointed at a
  personal account's public ECR). spore.host ships the recipe; the image is BYO —
  build it (`infra/amis/containers/<app>`) and bind it in `~/.spawn/catalog.yaml`
  or pass `--image`. `base_amis` stays so a bound image launches on the shared
  DCV base AMI.

- **catalog: online public-resolvability gate** (BYO-image model, spore-host#392).
  New `ResolvePublicImages()` anonymously HEAD-checks each shipped container
  image's manifest via the OCI registry v2 API (following the standard Bearer
  challenge — no Docker, no creds), rejecting any image that is private or not
  anonymously pullable. Wired into CI behind the `online` build tag (`go test
  -tags online ./catalog/`) so a dead/private/wrong-tag ref in the global catalog
  fails CI — the gap that let `chimerax:1.8` and dangling AMIs through before.

## [0.40.0] - 2026-06-27

### Added
- **catalog: image `visibility` (public/private) + inference** (BYO-image model,
  spore-host#392). New `AppEntry.Visibility` field and `ImageVisibility()`
  accessor: explicit value wins, else inferred from the registry
  (`public.ecr.aws/*` → public, private-ECR `*.dkr.ecr.*.amazonaws.com/*` →
  private, everything else → public). This underpins the per-account
  list/launch filter — public images surface for everyone, private images only
  for accounts that can pull them. `Validate()` now also rejects any non-public
  image in the shipped catalog (private images belong in a user's local overlay).
- **catalog: local overlay** (BYO-image model, spore-host#392). The embedded
  catalog can now be layered with a user overlay that adds apps or rebinds an
  existing app's image to one the user hosts (the only place private images
  belong). Path precedence: `SetOverlayPath` (e.g. a `--catalog` flag) >
  `$SPAWN_CATALOG` > `~/.spawn/catalog.yaml`; a missing default/env file is fine
  (opt-in), a malformed or explicitly-named-but-missing file is reported via the
  new `LoadError()` and falls back to embedded-only. New `SetOverlayPath`,
  `Reload`, and `LoadError`; overlay entries merge by name (overlay wins,
  case-insensitive) over the embedded baseline.

## [0.39.2] - 2026-06-27

### Changed
- **catalog: chimerax bumped to 1.12** (#290). 1.8 no longer exists on UCSF's
  download site; 1.12 is the current production release. The image must still be
  built/pushed (ChimeraX has a license-gated download — see
  infra/amis/containers/chimerax), so a chimerax launch isn't functional until
  that image is published.

## [0.39.1] - 2026-06-26

### Fixed
- **catalog: point app images at the real ECR Public registry** (#290). The
  paraview/chimerax `image:` prefixes are now `public.ecr.aws/f8g1e7l5/…` (the
  build account's default ECR Public alias) instead of the aspirational
  `public.ecr.aws/spore-host/…`, which does not resolve (a custom alias needs an
  async AWS approval). `paraview:5.13.2` is published and publicly pullable; the
  base AMI note now reflects the real owning account (942542972736).

## [0.39.0] - 2026-06-26

### Added
- **catalog: `Validate()` structural gate** (#290, #389). Returns one error per
  catalog defect with no AWS calls — every app is launchable (image or
  launch_command), no app reuses the deprecated per-app `amis` table, and each
  container app has a `tag_default` within `tags_available` plus a non-empty
  `base_amis`, with a unique image. Run in CI via the existing `go test ./...`
  (`TestValidate_EmbeddedCatalogClean`), so a #389-class bad entry can't merge.
  (ECR/AMI-visibility checks need AWS creds and live in a separate job.)

## [0.38.0] - 2026-06-26

### Added
- **catalog: container-based app model** (#290). `AppEntry` gains `Image`,
  `TagDefault`, `TagsAvailable`, and `BaseAMIs` (region → shared `spore-dcv-base`
  AMI). A containerized app runs `Image:tag` on the shared base AMI instead of a
  baked per-app AMI. New helpers: `AppEntry.ResolveTag(requested)` (validates a
  requested version against `TagsAvailable`, falling back to `TagDefault`) and
  `AppEntry.Containerized()`. paraview and chimerax are now container entries.

### Changed
- **catalog: an app is launchable via a container image OR a `launch_command`**
  (#290). GPU apps (paraview, chimerax) launch from their image CMD and no longer
  set `launch_command`; CPU apps keep it until they are containerized.

### Removed
- **catalog: deleted the per-app, per-region baked AMI tables** (#389). Every ID
  in them was found dangling or unshared from the launch account, and several were
  duplicated across apps (a paraview launch outside us-east-1 would have booted the
  chimerax image). The `amis` field remains on `AppEntry` for one release as a
  deprecated, must-be-empty escape hatch; new entries use `image` + `base_amis`.

## [0.37.1] - 2026-06-12

### Fixed
- i18n: removed stray `{{.Count}}`/`{{.Percent}}` template variables from eight
  `truffle.capacity.summary.*` labels in the es/fr/de/ja/pt translations. The
  truffle capacity command formats counts itself, so these strings supply only
  the label; the leftover variables made `i18n.T` (called with no template data)
  fall through to its error path and render `[truffle.capacity.summary.<key>]`
  in non-English locales. English was already corrected.

## [0.37.0] - 2026-06-12

### Added
- `update.CheckNow(tool, currentVersion) *Result` — a synchronous, ungated
  version check for explicit, user-initiated use (e.g. a `version` subcommand).
  Unlike `CheckAsync` it ignores the CI / `SPORE_NO_UPDATE_CHECK` / non-TTY
  suppressions and bypasses the 24h cache, so the caller always gets a fresh
  answer; returns nil when the GitHub releases API can't be reached.

## [0.36.0] - 2026-06-07

Latest tagged release. See the
[GitHub Releases](https://github.com/spore-host/libs/releases) for the contents
of this and earlier tags (`update`, `i18n`, `catalog`, `pricing` packages).

---

[Unreleased]: https://github.com/spore-host/libs/compare/v0.43.2...HEAD
[0.43.2]: https://github.com/spore-host/libs/compare/v0.43.1...v0.43.2
[0.43.1]: https://github.com/spore-host/libs/compare/v0.43.0...v0.43.1
[0.43.0]: https://github.com/spore-host/libs/compare/v0.42.0...v0.43.0
[0.42.0]: https://github.com/spore-host/libs/compare/v0.41.1...v0.42.0
[0.41.1]: https://github.com/spore-host/libs/compare/v0.41.0...v0.41.1
[0.41.0]: https://github.com/spore-host/libs/compare/v0.40.0...v0.41.0
[0.40.0]: https://github.com/spore-host/libs/compare/v0.39.2...v0.40.0
[0.39.2]: https://github.com/spore-host/libs/compare/v0.39.1...v0.39.2
[0.39.1]: https://github.com/spore-host/libs/compare/v0.39.0...v0.39.1
[0.39.0]: https://github.com/spore-host/libs/compare/v0.38.0...v0.39.0
[0.38.0]: https://github.com/spore-host/libs/compare/v0.37.1...v0.38.0
[0.37.1]: https://github.com/spore-host/libs/compare/v0.37.0...v0.37.1
[0.37.0]: https://github.com/spore-host/libs/compare/v0.36.0...v0.37.0
[0.36.0]: https://github.com/spore-host/libs/releases/tag/v0.36.0
