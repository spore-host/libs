# sporeconfig

Shared configuration base for the spore.host suite (spawn, truffle, lagotto,
spore-host-mcp). It resolves the settings every tool needs — AWS **profile**,
**region**, **account**, and default **output** format — from one place, with one
precedence order.

It is **SDK-free**: it resolves these as strings and never imports the AWS SDK.
Each tool turns the resolved strings into an `aws.Config` itself (pass
`Config.Profile` to `WithSharedConfigProfile` and `Config.Region` to `WithRegion`,
each only when non-empty). An empty value means "not configured" — the tool
passes no override and the AWS SDK's ambient resolution applies, so an
unconfigured suite behaves exactly as before.

## Precedence

Highest wins:

1. **CLI flag** — the `Flags` struct a caller fills from its cobra flags.
2. **Environment** — `SPORE_PROFILE`, `SPORE_REGION`, `SPORE_ACCOUNT`,
   `SPORE_OUTPUT`. As fallbacks (only when the `SPORE_*` var is unset),
   `AWS_PROFILE` and `AWS_REGION`/`AWS_DEFAULT_REGION` are honored so existing AWS
   setups keep working.
3. **Config file** — the `[spore]` table of `~/.config/spore/config.toml`
   (see below).
4. **Default** — `output = "table"`; everything else empty (→ ambient AWS chain).

## Config file

Location: `$XDG_CONFIG_HOME/spore/config.toml`, or `~/.config/spore/config.toml`
if `XDG_CONFIG_HOME` is unset. The file is **opt-in** — its absence is not an
error.

```toml
[spore]
profile = "spore-host-dev"   # AWS named profile; omit for the ambient chain
region  = "us-west-2"        # default AWS region
account = "435415984226"     # expected AWS account ID (optional; for guards/display)
output  = "table"            # default output format: table | json | yaml | csv

# Tools may add their own tables here; sporeconfig ignores them and each tool
# reads its own:
# [spawn]
# [lagotto]
```

## Usage

```go
sc, err := sporeconfig.Resolve(sporeconfig.Flags{
    Profile: profileFlag, // "" when the flag wasn't set
    Region:  regionFlag,
    Output:  outputFlag,
})
// err is non-nil only for a malformed config file; sc is still populated from
// the flag/env/default layers, so a caller may choose to proceed.

opts := []func(*awsconfig.LoadOptions) error{}
if sc.Region != "" {
    opts = append(opts, awsconfig.WithRegion(sc.Region))
}
if sc.Profile != "" {
    opts = append(opts, awsconfig.WithSharedConfigProfile(sc.Profile))
}
cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
```
