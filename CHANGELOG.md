# Changelog

All notable changes to **libs** (shared Go libraries for the spore.host tools)
are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Security
- **Bumped `golang.org/x/text` v0.21.0 → v0.39.0 — CVE-2026-56852 (HIGH)** (#35).
  A `norm.Iter` can enter an infinite loop on certain input. libs only imports
  `x/text/language`, so the vulnerable symbol isn't called and `govulncheck` reports
  nothing — but the dependency is present and it is the kind of thing that becomes
  reachable one import later.

  Worth stating why it sat here so long: all three CLIs had already been forced to
  v0.39.0 by *their own* security scans. libs, which they all import, had no
  security workflow, so it was the one place nobody was looking — the most-shared
  module in the suite was the least-scanned.

- **Added a security workflow** (#35). libs was the only Go repo in the suite
  without one: govulncheck (pinned `@v1.3.0` — v1.4.0 panics on generics), gitleaks
  over full history, Trivy fs + config, and Semgrep SAST, on PR/push/weekly.

  Semgrep is also what flags mutable action tags, which is why libs was the last
  repo still using floating `@v6` refs until they were pinned above — the rule
  lives in a workflow this repo didn't have.

- **Pinned GitHub Actions to commit SHAs** instead of floating tags
  (`actions/checkout@v6` → `@d23441a… # v6.1.0`, `setup-go`, `codecov-action`).
  A tag is mutable: `@v6` means "whatever v6 points at when the job runs", so the
  code CI executes — with a checkout of this repo and a token — could change with
  no commit here. Not hypothetical: `actions/checkout@v6` had already moved from
  the v6.0.3-era commit the sibling repos pin to a later one, silently.

  A test now enforces the pinning (full 40-hex SHA plus a `# vX.Y.Z` comment, so
  bumps stay reviewable).

- **Added a Dependabot config so those pins actually get bumped.** Pinning to a
  SHA closes the mutable-tag hole but opens a staleness one: a SHA never moves,
  including past a security fix, and unlike `@v6` nothing updates it for you — so
  pinning is only safe if something bumps the pins. This repo had no Dependabot
  config and has no security workflow, so nothing did, and nothing would have said
  so. Weekly, grouped into one PR, with a 7-day cooldown so a freshly-published
  action tag isn't proposed the day it ships. Dependabot rewrites the `# vX.Y.Z`
  comment alongside the SHA, so the pins stay readable and the test keeps passing.

  The group pattern is `*`, not the umbrella repo's `actions/*`: this repo also
  uses `codecov/codecov-action`, which `actions/*` would leave out. A test asserts
  that every action in every workflow is matched by some pattern, so adding a
  third-party action without widening the pattern fails CI rather than quietly
  going unmanaged.

### Changed
- **CI now fails on unformatted code (#30).** Nothing did before: the workflow had
  no formatting step, and this repo has no Makefile, so there wasn't even a local
  `fmt` target anyone might run. Three files in `i18n/` sat unformatted on `main`.

  The gate reports drift instead of fixing it — offenders listed with a diff —
  because `gofmt -w` rewrites files and exits 0, so a check built on it could only
  ever report success. The three drifted files are formatted (struct-field and
  map-literal alignment only; verified whitespace-only with `git diff -w`).
  Internal to the repo; no API change.

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
