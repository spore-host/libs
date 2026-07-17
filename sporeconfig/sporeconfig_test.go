package sporeconfig

import (
	"os"
	"path/filepath"
	"testing"
)

// clearEnv unsets every env var Resolve consults, so a test starts from a clean
// slate regardless of the developer's shell, and points XDG_CONFIG_HOME at a
// temp dir so no real ~/.config/spore is read.
func clearEnv(t *testing.T, xdgHome string) {
	t.Helper()
	for _, k := range []string{
		"SPORE_PROFILE", "SPORE_REGION", "SPORE_ACCOUNT", "SPORE_OUTPUT",
		"AWS_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("XDG_CONFIG_HOME", xdgHome)
}

// writeConfig writes a config.toml under xdgHome/spore/.
func writeConfig(t *testing.T, xdgHome, body string) {
	t.Helper()
	dir := filepath.Join(xdgHome, "spore")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolve_DefaultsWhenNothingSet(t *testing.T) {
	clearEnv(t, t.TempDir()) // temp XDG dir with no config file
	got, err := Resolve(Flags{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Profile != "" || got.Region != "" || got.Account != "" {
		t.Errorf("expected empty profile/region/account, got %+v", got)
	}
	if got.Output != DefaultOutput {
		t.Errorf("Output = %q, want %q", got.Output, DefaultOutput)
	}
}

func TestResolve_Precedence(t *testing.T) {
	xdg := t.TempDir()
	writeConfig(t, xdg, `
[spore]
profile = "file-profile"
region  = "file-region"
account = "file-account"
output  = "yaml"
`)

	t.Run("file layer wins when no flag/env", func(t *testing.T) {
		clearEnv(t, xdg)
		got, _ := Resolve(Flags{})
		if got.Profile != "file-profile" || got.Region != "file-region" ||
			got.Account != "file-account" || got.Output != "yaml" {
			t.Errorf("file layer not applied: %+v", got)
		}
	})

	t.Run("env beats file", func(t *testing.T) {
		clearEnv(t, xdg)
		t.Setenv("SPORE_PROFILE", "env-profile")
		t.Setenv("SPORE_REGION", "env-region")
		got, _ := Resolve(Flags{})
		if got.Profile != "env-profile" || got.Region != "env-region" {
			t.Errorf("env should beat file, got %+v", got)
		}
		if got.Account != "file-account" { // unset in env → still file
			t.Errorf("account should fall through to file, got %q", got.Account)
		}
	})

	t.Run("flag beats env and file", func(t *testing.T) {
		clearEnv(t, xdg)
		t.Setenv("SPORE_PROFILE", "env-profile")
		got, _ := Resolve(Flags{Profile: "flag-profile", Output: "json"})
		if got.Profile != "flag-profile" {
			t.Errorf("flag should beat env, got %q", got.Profile)
		}
		if got.Output != "json" {
			t.Errorf("flag output should win, got %q", got.Output)
		}
	})
}

func TestResolve_AWSEnvFallback(t *testing.T) {
	clearEnv(t, t.TempDir())
	// No SPORE_* set; AWS_* should be honored at the env layer.
	t.Setenv("AWS_PROFILE", "aws-prof")
	t.Setenv("AWS_REGION", "us-west-2")
	got, _ := Resolve(Flags{})
	if got.Profile != "aws-prof" {
		t.Errorf("AWS_PROFILE fallback not applied, got %q", got.Profile)
	}
	if got.Region != "us-west-2" {
		t.Errorf("AWS_REGION fallback not applied, got %q", got.Region)
	}
}

func TestResolve_SporeEnvBeatsAWSEnv(t *testing.T) {
	clearEnv(t, t.TempDir())
	t.Setenv("AWS_PROFILE", "aws-prof")
	t.Setenv("SPORE_PROFILE", "spore-prof")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("SPORE_REGION", "eu-west-1")
	got, _ := Resolve(Flags{})
	if got.Profile != "spore-prof" {
		t.Errorf("SPORE_PROFILE should beat AWS_PROFILE, got %q", got.Profile)
	}
	if got.Region != "eu-west-1" {
		t.Errorf("SPORE_REGION should beat AWS_REGION, got %q", got.Region)
	}
}

func TestResolve_AWSDefaultRegionFallback(t *testing.T) {
	clearEnv(t, t.TempDir())
	// Only AWS_DEFAULT_REGION set (no AWS_REGION / SPORE_REGION).
	t.Setenv("AWS_DEFAULT_REGION", "ap-south-1")
	got, _ := Resolve(Flags{})
	if got.Region != "ap-south-1" {
		t.Errorf("AWS_DEFAULT_REGION fallback not applied, got %q", got.Region)
	}
}

func TestResolve_MissingFileIsNotError(t *testing.T) {
	clearEnv(t, t.TempDir()) // dir exists, no config.toml in it
	if _, err := Resolve(Flags{}); err != nil {
		t.Errorf("missing file should not error, got %v", err)
	}
}

func TestResolve_MalformedFileErrorsButStillResolves(t *testing.T) {
	xdg := t.TempDir()
	writeConfig(t, xdg, "this is not = valid toml [[[")
	clearEnv(t, xdg)
	t.Setenv("SPORE_REGION", "us-east-2")
	got, err := Resolve(Flags{Profile: "flag-p"})
	if err == nil {
		t.Error("expected an error for malformed TOML")
	}
	// Flag/env/default layers still populate despite the bad file.
	if got.Profile != "flag-p" || got.Region != "us-east-2" || got.Output != DefaultOutput {
		t.Errorf("resolve should still apply flag/env/default on bad file, got %+v", got)
	}
}

func TestConfigPath_XDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdgtest")
	p, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/tmp/xdgtest", "spore", "config.toml"); p != want {
		t.Errorf("ConfigPath = %q, want %q", p, want)
	}
}

func TestConfigPath_HomeDefault(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	p, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "spore", "config.toml"); p != want {
		t.Errorf("ConfigPath = %q, want %q", p, want)
	}
}
