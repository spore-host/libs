# spore-host/libs

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
