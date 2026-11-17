# spore-host/libs

[![CI](https://github.com/spore-host/libs/actions/workflows/ci.yml/badge.svg)](https://github.com/spore-host/libs/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/spore-host/libs/branch/main/graph/badge.svg)](https://codecov.io/gh/spore-host/libs)
[![Go Reference](https://pkg.go.dev/badge/github.com/spore-host/libs.svg)](https://pkg.go.dev/github.com/spore-host/libs)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

Shared Go packages used by [truffle](https://github.com/spore-host/truffle), [spawn](https://github.com/spore-host/spawn), and [lagotto](https://github.com/spore-host/lagotto).

## Packages

### `github.com/spore-host/libs/catalog`
Application catalog — a registry of streamable research applications with hardware requirements and AMI IDs.

### `github.com/spore-host/libs/i18n`
Internationalization support for CLI output. Supports English, Spanish, French, German, Japanese, and Portuguese.

### `github.com/spore-host/libs/pricing`
EC2 on-demand pricing data and cost estimation utilities.

### `github.com/spore-host/libs/tags`
The `spore-host:*` resource tags (`managed-by`, `scope`, `created`, ...) that every tool puts on what it creates, so leave-no-trace cleanup can find it. Dependency-free; per-service converters in `tags/ec2tags`, `tags/iamtags`, `tags/dynamodbtags`, `tags/snstags`, `tags/s3tags`, `tags/schedulertags`.

### `github.com/spore-host/libs/tags/tagtest`
An AWS SDK v2 middleware for unit tests that fails the test when a create call lacks the `spore-host:managed-by` tag.

### `github.com/spore-host/libs/lifecycle`
The shared-account lease (`spore-host-lifecycle` DynamoDB table) that lets a tool tear down what it created without racing another user's setup. Creators call `MarkActive`; backstops call `Observe`, `Claim`, `Lease.Step`/`Finish`/`Release`, and `CloseTableIfEmpty`. `lifecycle/lifecycletest` is an in-memory DynamoDB for testing against it.

### `github.com/spore-host/libs/footprint`
The JSON schema of `<tool> footprint -o json`, the delete guard (`Deletable`), and the dependency-ordered teardown executor (`NewPlan`, `Execute` in `report` or `reap` mode).

### `github.com/spore-host/libs/lastmile`
The lazy CLI last mile: called after a tool's main command, it reports stale teardowns to resume, residue left once no lifecycle item remains, empty lifecycle tables and, for a tool with no backstop and full visibility, an idle teardown. Report mode only (`SPORE_LASTMILE=off|report`); at most once per 6 h per tool, account and scope, within a 30 s budget, and never fails the command.

## Usage

```go
import (
    "github.com/spore-host/libs/i18n"
    "github.com/spore-host/libs/catalog"
    "github.com/spore-host/libs/pricing"
)
```

```bash
go get github.com/spore-host/libs@latest
```

## License

Apache 2.0 — Copyright 2025-2026 Scott Friedman.
