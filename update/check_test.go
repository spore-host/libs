package update

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCompareSemver(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"0.37.1", "0.37.0", 1},
		{"0.37.0", "0.37.1", -1},
		{"0.37.0", "0.37.0", 0},
		{"1.0.0", "0.99.99", 1},
		{"0.38.0", "0.37.1", 1},
		{"v0.37.1", "0.37.0", 1},
		{"0.37.1-rc1", "0.37.0", 1},
	}
	for _, tt := range tests {
		if got := compareSemver(tt.a, tt.b); got != tt.want {
			t.Errorf("compareSemver(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestParseSemver(t *testing.T) {
	tests := []struct {
		input string
		want  [3]int
	}{
		{"0.37.1", [3]int{0, 37, 1}},
		{"v1.2.3", [3]int{1, 2, 3}},
		{"0.37", [3]int{0, 37, 0}},
		{"1", [3]int{1, 0, 0}},
		{"0.37.1-rc1", [3]int{0, 37, 1}},
	}
	for _, tt := range tests {
		if got := parseSemver(tt.input); got != tt.want {
			t.Errorf("parseSemver(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestResult_HasUpdate(t *testing.T) {
	tests := []struct {
		name   string
		result *Result
		want   bool
	}{
		{"nil", nil, false},
		{"same version", &Result{CurrentVersion: "0.37.0", LatestVersion: "0.37.0"}, false},
		{"newer available", &Result{CurrentVersion: "0.37.0", LatestVersion: "0.37.1"}, true},
		{"current is newer", &Result{CurrentVersion: "0.38.0", LatestVersion: "0.37.1"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.result.HasUpdate(); got != tt.want {
				t.Errorf("HasUpdate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResult_Message(t *testing.T) {
	r := &Result{
		CurrentVersion: "0.37.0",
		LatestVersion:  "0.38.0",
		UpdateURL:      "https://github.com/spore-host/truffle/releases/tag/v0.38.0",
	}
	msg := r.Message()
	if msg == "" {
		t.Fatal("expected non-empty message for available update")
	}
	if !contains(msg, "0.38.0") || !contains(msg, "truffle") {
		t.Errorf("message missing expected content: %s", msg)
	}

	noUpdate := &Result{CurrentVersion: "0.38.0", LatestVersion: "0.38.0"}
	if noUpdate.Message() != "" {
		t.Error("expected empty message when no update available")
	}
}

func TestCacheReadWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-cache")

	result := &Result{
		CurrentVersion: "0.37.0",
		LatestVersion:  "0.38.0",
		UpdateURL:      "https://example.com",
	}

	writeCache(path, result)

	got := readCache(path, "0.37.0")
	if got == nil {
		t.Fatal("expected non-nil cached result")
	}
	if got.LatestVersion != "0.38.0" {
		t.Errorf("cached LatestVersion = %q, want %q", got.LatestVersion, "0.38.0")
	}

	// Different version should miss cache
	if readCache(path, "0.37.1") != nil {
		t.Error("expected nil for different currentVersion")
	}
}

func TestCacheExpiry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-cache")

	entry := cacheEntry{
		CheckedAt:      time.Now().Add(-25 * time.Hour).Unix(),
		LatestVersion:  "0.38.0",
		CurrentVersion: "0.37.0",
		UpdateURL:      "https://example.com",
	}
	data, _ := json.Marshal(entry)
	_ = os.WriteFile(path, data, 0644)

	if readCache(path, "0.37.0") != nil {
		t.Error("expected nil for expired cache")
	}
}

func TestCheckAsync_DisabledByEnv(t *testing.T) {
	t.Setenv("SPORE_NO_UPDATE_CHECK", "1")
	ch := CheckAsync("truffle", "0.37.0")
	result := <-ch
	if result != nil {
		t.Error("expected nil when SPORE_NO_UPDATE_CHECK is set")
	}
}

func TestCheckAsync_DisabledInCI(t *testing.T) {
	t.Setenv("CI", "true")
	t.Setenv("SPORE_NO_UPDATE_CHECK", "")
	ch := CheckAsync("truffle", "0.37.0")
	result := <-ch
	if result != nil {
		t.Error("expected nil when CI is set")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
