# Changelog

All notable changes to **libs** (shared Go libraries for the spore.host tools)
are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/spore-host/libs/compare/v0.39.1...HEAD
[0.39.1]: https://github.com/spore-host/libs/compare/v0.39.0...v0.39.1
[0.39.0]: https://github.com/spore-host/libs/compare/v0.38.0...v0.39.0
[0.38.0]: https://github.com/spore-host/libs/compare/v0.37.1...v0.38.0
[0.37.1]: https://github.com/spore-host/libs/compare/v0.37.0...v0.37.1
[0.37.0]: https://github.com/spore-host/libs/compare/v0.36.0...v0.37.0
[0.36.0]: https://github.com/spore-host/libs/releases/tag/v0.36.0
