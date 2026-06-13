# Changelog

All notable changes to **libs** (shared Go libraries for the spore.host tools)
are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/spore-host/libs/compare/v0.37.0...HEAD
[0.37.0]: https://github.com/spore-host/libs/compare/v0.36.0...v0.37.0
[0.36.0]: https://github.com/spore-host/libs/releases/tag/v0.36.0
