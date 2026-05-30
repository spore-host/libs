# spore-host/libs

[![CI](https://github.com/spore-host/libs/actions/workflows/ci.yml/badge.svg)](https://github.com/spore-host/libs/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/spore-host/libs)](https://goreportcard.com/report/github.com/spore-host/libs)
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
