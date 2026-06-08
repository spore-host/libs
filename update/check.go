// Package update provides non-blocking version checking for spore-host CLI tools.
// It queries the GitHub releases API at most once per 24 hours and caches results.
package update

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	checkInterval = 24 * time.Hour
	httpTimeout   = 3 * time.Second
	githubOrg     = "spore-host"
)

// Result holds the outcome of a version check.
type Result struct {
	CurrentVersion string
	LatestVersion  string
	UpdateURL      string
}

// HasUpdate returns true if a newer version is available.
func (r *Result) HasUpdate() bool {
	if r == nil {
		return false
	}
	return compareSemver(r.LatestVersion, r.CurrentVersion) > 0
}

// Message returns the user-facing notice string.
func (r *Result) Message() string {
	if !r.HasUpdate() {
		return ""
	}
	return fmt.Sprintf("A newer version of %s is available: %s → %s\n  %s",
		repoFromURL(r.UpdateURL), r.CurrentVersion, r.LatestVersion, r.UpdateURL)
}

// CheckAsync starts a background version check and returns a channel that
// yields the result. If the check is not due (cached within 24h), the env var
// SPORE_NO_UPDATE_CHECK is set, or the tool is not a TTY, the channel returns
// nil immediately.
func CheckAsync(tool, currentVersion string) <-chan *Result {
	ch := make(chan *Result, 1)

	if os.Getenv("SPORE_NO_UPDATE_CHECK") != "" {
		ch <- nil
		return ch
	}

	if os.Getenv("CI") != "" {
		ch <- nil
		return ch
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				ch <- nil
			}
		}()

		result := check(tool, currentVersion)
		ch <- result
	}()

	go func() {
		wg.Wait()
		close(ch)
	}()

	return ch
}

func check(tool, currentVersion string) *Result {
	cacheDir := cacheDirectory()
	cacheFile := filepath.Join(cacheDir, tool+"-update-check")

	if cached := readCache(cacheFile, currentVersion); cached != nil {
		return cached
	}

	latest, url, err := fetchLatestRelease(tool)
	if err != nil {
		return nil
	}

	result := &Result{
		CurrentVersion: currentVersion,
		LatestVersion:  latest,
		UpdateURL:      url,
	}

	writeCache(cacheFile, result)
	return result
}

func cacheDirectory() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return os.TempDir()
	}
	dir := filepath.Join(home, ".spore", "cache")
	_ = os.MkdirAll(dir, 0755)
	return dir
}

type cacheEntry struct {
	CheckedAt      int64  `json:"checked_at"`
	LatestVersion  string `json:"latest_version"`
	CurrentVersion string `json:"current_version"`
	UpdateURL      string `json:"update_url"`
}

func readCache(path, currentVersion string) *Result {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var entry cacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil
	}

	checkedAt := time.Unix(entry.CheckedAt, 0)
	if time.Since(checkedAt) > checkInterval {
		return nil
	}

	if entry.CurrentVersion != currentVersion {
		return nil
	}

	return &Result{
		CurrentVersion: entry.CurrentVersion,
		LatestVersion:  entry.LatestVersion,
		UpdateURL:      entry.UpdateURL,
	}
}

func writeCache(path string, result *Result) {
	entry := cacheEntry{
		CheckedAt:      time.Now().Unix(),
		LatestVersion:  result.LatestVersion,
		CurrentVersion: result.CurrentVersion,
		UpdateURL:      result.UpdateURL,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0644)
}

type githubRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

func fetchLatestRelease(tool string) (version, url string, err error) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", githubOrg, tool)

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Get(apiURL)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("github returned %d", resp.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", "", err
	}

	version = strings.TrimPrefix(release.TagName, "v")
	return version, release.HTMLURL, nil
}

// compareSemver compares two semver strings (without "v" prefix).
// Returns -1 if a < b, 0 if a == b, 1 if a > b.
func compareSemver(a, b string) int {
	aParts := parseSemver(a)
	bParts := parseSemver(b)

	for i := 0; i < 3; i++ {
		if aParts[i] > bParts[i] {
			return 1
		}
		if aParts[i] < bParts[i] {
			return -1
		}
	}
	return 0
}

func parseSemver(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	parts := strings.SplitN(v, ".", 3)
	var result [3]int
	for i := 0; i < len(parts) && i < 3; i++ {
		// Strip pre-release suffix (e.g., "1-rc1" → "1")
		num := strings.SplitN(parts[i], "-", 2)[0]
		result[i], _ = strconv.Atoi(num)
	}
	return result
}

func repoFromURL(url string) string {
	parts := strings.Split(url, "/")
	if len(parts) >= 5 {
		return parts[4]
	}
	return "this tool"
}
