// Package sporeconfig resolves the settings shared across the spore.host suite
// (spawn, truffle, lagotto, spore-host-mcp): the AWS named profile, default
// region, expected account, and default output format.
//
// It is deliberately SDK-free — it resolves these as strings and never imports
// the AWS SDK, so it stays as dependency-light as the rest of libs. Each tool
// turns the resolved strings into an aws.Config itself (a two-line helper): pass
// Config.Profile to WithSharedConfigProfile and Config.Region to WithRegion, both
// only when non-empty. An empty value means "not configured" — the tool passes
// no override and the AWS SDK's own ambient resolution (AWS_PROFILE / AWS_REGION /
// ~/.aws) applies underneath, so an unconfigured suite behaves exactly as before.
//
// Precedence (highest first): CLI flag > environment variable > config file >
// built-in default. This matches spawn's existing config loaders.
//
//	flag:    the Flags struct a caller fills from its cobra flags ("" = unset)
//	env:     SPORE_PROFILE, SPORE_REGION, SPORE_ACCOUNT, SPORE_OUTPUT; plus
//	         AWS_PROFILE / AWS_REGION (and AWS_DEFAULT_REGION) as env-layer
//	         fallbacks for profile/region so existing AWS setups keep working
//	file:    the [spore] table of the config file (see ConfigPath)
//	default: Output = "table"; the rest "" (→ ambient SDK resolution)
package sporeconfig

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// DefaultOutput is the built-in default output format.
const DefaultOutput = "table"

// Config is the resolved shared configuration — the values from the file's
// [spore] table after flag/env/file/default precedence is applied.
type Config struct {
	Profile string // AWS named profile; "" = ambient credential chain
	Region  string // default AWS region; "" = ambient/SDK-default region
	Account string // expected AWS account ID (optional; for display/guards)
	Output  string // default output format: table|json|yaml|csv
}

// Flags carries the values a caller read from its CLI flags. Any empty field is
// treated as "not set" and falls through to the next precedence layer.
type Flags struct {
	Profile string
	Region  string
	Account string
	Output  string
}

// fileConfig mirrors the on-disk TOML. Only the [spore] table is parsed here;
// per-tool tables ([spawn], [lagotto], …) are left to each tool and ignored by
// toml's decoder (unknown top-level keys are not an error for our struct).
type fileConfig struct {
	Spore sporeTable `toml:"spore"`
}

type sporeTable struct {
	Profile string `toml:"profile"`
	Region  string `toml:"region"`
	Account string `toml:"account"`
	Output  string `toml:"output"`
}

// ConfigDir returns the directory holding the shared config file:
// $XDG_CONFIG_HOME/spore if XDG_CONFIG_HOME is set, else ~/.config/spore.
func ConfigDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "spore"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "spore"), nil
}

// ConfigPath returns the shared config file path: ConfigDir()/config.toml.
func ConfigPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.toml"), nil
}

// Resolve returns the shared Config using flag > env > file > default precedence.
//
// A missing config file is not an error (the whole layer is opt-in). A malformed
// config file IS returned as an error, but the returned Config is still populated
// from the flag/env/default layers so a caller can choose to proceed on ambient
// credentials if it wants to treat the file as advisory.
func Resolve(flags Flags) (Config, error) {
	// Layer 3 (file), read once. Missing → empty table, no error.
	file, ferr := loadFileTable()

	// Layer 2 (env). AWS_* are honored as fallbacks so existing setups work even
	// without SPORE_* set; SPORE_* takes precedence over AWS_* within this layer.
	envProfile := firstNonEmpty(os.Getenv("SPORE_PROFILE"), os.Getenv("AWS_PROFILE"))
	envRegion := firstNonEmpty(os.Getenv("SPORE_REGION"), os.Getenv("AWS_REGION"), os.Getenv("AWS_DEFAULT_REGION"))
	envAccount := os.Getenv("SPORE_ACCOUNT")
	envOutput := os.Getenv("SPORE_OUTPUT")

	cfg := Config{
		Profile: firstNonEmpty(flags.Profile, envProfile, file.Profile),
		Region:  firstNonEmpty(flags.Region, envRegion, file.Region),
		Account: firstNonEmpty(flags.Account, envAccount, file.Account),
		Output:  firstNonEmpty(flags.Output, envOutput, file.Output, DefaultOutput),
	}
	return cfg, ferr
}

// loadFileTable reads the [spore] table from the shared config file. A missing
// file yields a zero table and nil error (opt-in); a malformed file yields a
// zero table and a wrapped error.
func loadFileTable() (sporeTable, error) {
	path, err := ConfigPath()
	if err != nil {
		return sporeTable{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return sporeTable{}, nil // opt-in: no file is fine
		}
		return sporeTable{}, fmt.Errorf("read shared config %s: %w", path, err)
	}
	var fc fileConfig
	if err := toml.Unmarshal(data, &fc); err != nil {
		return sporeTable{}, fmt.Errorf("parse shared config %s: %w", path, err)
	}
	return fc.Spore, nil
}

// firstNonEmpty returns the first non-empty string, or "" if all are empty.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
