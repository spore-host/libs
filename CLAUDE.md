# CLAUDE.md — libs

`libs` holds the shared Go libraries for the spore.host tools (i18n, catalog,
pricing, update-check, …), imported by spawn / truffle / lagotto.

## Versioning & changelog (required)

This project follows **[Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html)**
and keeps a **[Keep a Changelog](https://keepachangelog.com/en/1.1.0/)**-format
`CHANGELOG.md` at the repo root. (Spore.host-wide policy — every repo.)

**Every change that affects consumers updates `CHANGELOG.md`** in the same PR,
under `## [Unreleased]` in the right group (`Added` / `Changed` / `Deprecated` /
`Removed` / `Fixed` / `Security`; `Documentation` for docs-only). Write the
user-visible effect, not the implementation; reference the issue/PR.

**API note:** libs is a Go module consumed by other repos. A change to an
exported symbol's behavior or signature is a consumer-facing change — changelog
it, and treat a breaking signature change as a SemVer-major bump (pre-1.0:
minor).

**On release:**

1. Promote `## [Unreleased]` → `## [X.Y.Z] - YYYY-MM-DD`, open a fresh
   `## [Unreleased]`, update the comparison links.
2. Pick `X.Y.Z` by SemVer (MAJOR breaking / MINOR feature / PATCH fix; pre-1.0
   breaking → MINOR).
3. Tag `vX.Y.Z`. libs is a library (no GoReleaser) — the **tag is the release**;
   consumers bump their `require` and pick it up.

## Build & test

- `go test ./...` — unit tests
- `go vet ./...`
