package update

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// releasesServer stands in for the GitHub releases API and counts the requests
// it receives, so a test can assert whether CheckAsync made the network call.
func releasesServer(t *testing.T, tag string) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"tag_name":"` + tag + `","html_url":"https://github.com/spore-host/truffle/releases/tag/` + tag + `"}`))
	}))
	t.Cleanup(srv.Close)
	old := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = old })
	return &hits
}

// isolateCheck gives CheckAsync an empty cache, clears both env suppressors and
// fixes the answer of the stderr-terminal seam. It returns the cache file path.
func isolateCheck(t *testing.T, terminal bool) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	t.Setenv("SPORE_NO_UPDATE_CHECK", "")
	t.Setenv("CI", "")
	old := stderrIsTerminal
	stderrIsTerminal = func() bool { return terminal }
	t.Cleanup(func() { stderrIsTerminal = old })
	return filepath.Join(home, ".spore", "cache", "truffle-update-check")
}

// A batch harness captures stderr, so the notice would land in its logs on every
// invocation (libs#69). With stderr not a terminal there must be no notice, no
// network request and no cache write.
func TestCheckAsync_NonTerminalStderr_NoCheck(t *testing.T) {
	cacheFile := isolateCheck(t, false)
	hits := releasesServer(t, "v9.9.9")

	if result := <-CheckAsync("truffle", "0.37.0"); result != nil {
		t.Errorf("CheckAsync yielded %+v with stderr not a terminal, want nil", result)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("CheckAsync made %d release request(s) with stderr not a terminal, want 0", n)
	}
	if _, err := os.Stat(cacheFile); !os.IsNotExist(err) {
		t.Errorf("CheckAsync touched the cache with stderr not a terminal (stat err %v)", err)
	}
}

// The other half: with stderr a terminal and no suppressor set, the check runs
// and yields a result whose notice names the newer version.
func TestCheckAsync_TerminalStderr_YieldsNotice(t *testing.T) {
	cacheFile := isolateCheck(t, true)
	hits := releasesServer(t, "v9.9.9")

	result := <-CheckAsync("truffle", "0.37.0")
	if !result.HasUpdate() {
		t.Fatalf("CheckAsync yielded %+v with stderr a terminal, want an available update", result)
	}
	if msg := result.Message(); !strings.Contains(msg, "9.9.9") || !strings.Contains(msg, "truffle") {
		t.Errorf("notice %q does not name truffle 9.9.9", msg)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("CheckAsync made %d release request(s), want 1", n)
	}
	if _, err := os.Stat(cacheFile); err != nil {
		t.Errorf("CheckAsync did not write the cache: %v", err)
	}
}

// Both env suppressors still work on a terminal, where nothing else would
// suppress the check, and they skip the request, not just the result.
func TestCheckAsync_EnvSuppressors(t *testing.T) {
	for _, tc := range []struct{ name, key, val string }{
		{"SPORE_NO_UPDATE_CHECK", "SPORE_NO_UPDATE_CHECK", "1"},
		{"SPORE_NO_UPDATE_CHECK any value", "SPORE_NO_UPDATE_CHECK", "no"},
		{"CI", "CI", "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateCheck(t, true)
			hits := releasesServer(t, "v9.9.9")
			t.Setenv(tc.key, tc.val)

			if result := <-CheckAsync("truffle", "0.37.0"); result != nil {
				t.Errorf("CheckAsync yielded %+v with %s=%q, want nil", result, tc.key, tc.val)
			}
			if n := hits.Load(); n != 0 {
				t.Errorf("CheckAsync made %d release request(s) with %s=%q, want 0", n, tc.key, tc.val)
			}
		})
	}
}

// isTerminal is the real seam behind stderrIsTerminal: a regular file and a pipe,
// the two things a harness redirects stderr to, are not terminals.
func TestIsTerminal_FileAndPipe(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if isTerminal(f) {
		t.Error("isTerminal(regular file) = true, want false")
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()
	if isTerminal(w) {
		t.Error("isTerminal(pipe) = true, want false")
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

// TestCheckNow_NotGatedAndBypassesCache verifies CheckNow does a fresh fetch
// even when CI / SPORE_NO_UPDATE_CHECK are set (it's an explicit, user-initiated
// check, unlike CheckAsync) and returns the result from the releases endpoint.
func TestCheckNow_NotGatedAndBypassesCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9","html_url":"https://example.test/releases/v9.9.9"}`))
	}))
	defer srv.Close()

	old := githubAPIBase
	githubAPIBase = srv.URL
	defer func() { githubAPIBase = old }()

	// Both suppressions on — CheckAsync would return nil; CheckNow must not.
	t.Setenv("CI", "true")
	t.Setenv("SPORE_NO_UPDATE_CHECK", "1")
	// Point the cache at a temp dir so we don't touch the real one.
	t.Setenv("HOME", t.TempDir())

	res := CheckNow("spawn", "0.1.0")
	if res == nil {
		t.Fatal("CheckNow returned nil despite a reachable endpoint (it must not be env-gated)")
	}
	if res.LatestVersion != "9.9.9" {
		t.Errorf("LatestVersion = %q, want 9.9.9", res.LatestVersion)
	}
	if !res.HasUpdate() {
		t.Error("HasUpdate() should be true (9.9.9 > 0.1.0)")
	}
}

// TestCheckNow_NilOnFetchError verifies CheckNow returns nil (not a panic) when
// the endpoint errors, so the caller can render "couldn't check".
func TestCheckNow_NilOnFetchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	old := githubAPIBase
	githubAPIBase = srv.URL
	defer func() { githubAPIBase = old }()
	t.Setenv("HOME", t.TempDir())

	if res := CheckNow("spawn", "0.1.0"); res != nil {
		t.Errorf("CheckNow should return nil on a 500, got %+v", res)
	}
}
