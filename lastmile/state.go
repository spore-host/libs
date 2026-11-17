package lastmile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// stateVersion is the version of the state file format.
const stateVersion = 1

// maxStateSize caps how much of the state file is read. A file larger than
// this is not one Run wrote, and is reset.
const maxStateSize = 1 << 20

// state is the content of the state file.
type state struct {
	Version int `json:"version"`
	// Runs is keyed by stateKey(tool, account, scope).
	Runs map[string]scopeState `json:"runs"`
}

// scopeState is the rate-limit record of one tool, account and scope.
type scopeState struct {
	// LastRun is when a run last checked the scope to the end. Zero if never.
	LastRun time.Time `json:"last_run,omitzero"`
	// LastAttempt is when a run last started on the scope.
	LastAttempt time.Time `json:"last_attempt,omitzero"`
	// Incomplete says why the last attempt did not finish; empty if it did.
	Incomplete string `json:"incomplete,omitempty"`
}

// stateKey is the key of a scope's record in state.Runs.
func stateKey(tool, account, scope string) string {
	return tool + "/" + account + "/" + scope
}

// DefaultStatePath is ~/.spore-host/lifecycle.json.
func DefaultStatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("lastmile: home directory: %w", err)
	}
	return filepath.Join(home, ".spore-host", "lifecycle.json"), nil
}

// loadState reads the state file at path. A missing file is an empty state
// and no error. A file that cannot be read or parsed, or has another
// version, is also returned as an empty state, with an error saying why it
// was reset.
func loadState(path string) (*state, error) {
	empty := &state{Version: stateVersion, Runs: map[string]scopeState{}}
	f, err := os.Open(path) // #nosec G304 -- the caller's own state file
	if errors.Is(err, fs.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, fmt.Errorf("lastmile: state file %s unreadable, reset: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxStateSize+1))
	if err != nil {
		return empty, fmt.Errorf("lastmile: state file %s unreadable, reset: %w", path, err)
	}
	if len(b) > maxStateSize {
		return empty, fmt.Errorf("lastmile: state file %s is over %d bytes, reset", path, maxStateSize)
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		return empty, fmt.Errorf("lastmile: state file %s is corrupt, reset: %w", path, err)
	}
	if s.Version != stateVersion {
		return empty, fmt.Errorf("lastmile: state file %s has version %d, this build reads %d; reset", path, s.Version, stateVersion)
	}
	if s.Runs == nil {
		s.Runs = map[string]scopeState{}
	}
	return &s, nil
}

// saveState writes s to path atomically: to a temporary file in the same
// directory, synced and renamed over path, so a reader sees the old file or
// the new one and never a partial write. The file is mode 0600; its directory
// is created 0700 if missing.
func saveState(path string, s *state) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("lastmile: state directory %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("lastmile: encode state: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".lifecycle-*.json.tmp")
	if err != nil {
		return fmt.Errorf("lastmile: state file: %w", err)
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("lastmile: state file: %w", err)
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("lastmile: write state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("lastmile: sync state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("lastmile: close state file: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("lastmile: replace state file %s: %w", path, err)
	}
	return nil
}

// due reports whether a scope last run at last is due again at now.
func due(last, now time.Time, minInterval time.Duration) bool {
	return last.IsZero() || last.After(now) || now.Sub(last) >= minInterval
}
