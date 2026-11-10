package prdash

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// StateDir returns the directory holding persistent prdash repository state files,
// following the XDG base directory specification on Unix.
func StateDir() (string, error) {
	var base string
	switch {
	case os.Getenv("XDG_STATE_HOME") != "":
		base = os.Getenv("XDG_STATE_HOME")
	case runtime.GOOS == "windows" || runtime.GOOS == "darwin":
		dir, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		base = dir
	default:
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(base, "prdash")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create state dir: %w", err)
	}
	return dir, nil
}

// DefaultStateFile returns the default persistent state file path for repoFullName.
func DefaultStateFile(repoFullName string) (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	slug := sanitizeID(strings.ReplaceAll(strings.TrimSpace(repoFullName), "/", "__"))
	if slug == "" {
		slug = "default"
	}
	return filepath.Join(dir, "state-"+slug+".json"), nil
}

type persistedState struct {
	InstructionClaims map[string]string `json:"instruction_claims,omitempty"`
	Revision          int64             `json:"revision"`
	Repository        RepositoryInfo    `json:"repository"`
	ModelConfig       ModelPresetConfig `json:"model_config"`
	Controller        Session           `json:"controller_session"`
	Worktrees         []Worktree        `json:"worktrees"`
	Sessions          []Session         `json:"sessions"`
	PullRequests      []PullRequest     `json:"pull_requests"`
	Notifications     []Notification    `json:"notifications"`
	Seq               int64             `json:"seq"`
}

func (s *Service) saveStateFileLocked() error {
	if s.stateFile == "" {
		return nil
	}
	if s.transaction == nil {
		return errors.New("state write requires a transaction")
	}
	// Detect a writer that did not honor the sidecar lock, including same-revision edits.
	current, err := os.ReadFile(s.stateFile)
	if errors.Is(err, os.ErrNotExist) && s.stateRev == 0 {
		current = nil
		err = nil
	}
	if err != nil {
		return err
	}
	var hash [sha256.Size]byte
	if current != nil {
		hash = sha256.Sum256(current)
	}
	if hash != s.stateHash {
		return ErrStateConflict
	}
	payload := s.captureStateLocked()
	payload.Revision = s.stateRev + 1
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("serialize state: %w", err)
	}
	if err := writeAtomicState(s.stateFile, data); err != nil {
		return err
	}

	s.stateRev = payload.Revision
	s.stateHash = sha256.Sum256(data)
	return nil
}

func (s *Service) loadStateFileLocked() (bool, error) {
	if s.stateFile == "" {
		return false, nil
	}
	data, err := os.ReadFile(s.stateFile)
	if errors.Is(err, os.ErrNotExist) && s.stateRev == 0 {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: read state: %w", ErrStatePersistence, err)
	}
	var payload persistedState
	if err := json.Unmarshal(data, &payload); err != nil {
		return false, fmt.Errorf("%w: %w", ErrStateCorrupt, err)
	}
	if payload.Revision <= 0 || payload.Controller.ID == "" {
		return false, fmt.Errorf("%w: missing revision or controller", ErrStateCorrupt)
	}
	hash := sha256.Sum256(data)
	if payload.Revision < s.stateRev || payload.Revision == s.stateRev && hash != s.stateHash {
		return false, ErrStateConflict
	}
	if payload.Revision == s.stateRev {
		return false, nil
	}
	s.restoreStateLocked(payload)
	s.stateHash = hash
	return true, nil
}

func (s *Service) reloadStateFileIfChangedLocked() bool {
	if s.transaction != nil {
		return false
	}
	changed, err := s.loadStateFileLocked()
	s.stateErr = err
	return changed
}

// WatchStateFile monitors the configured state file for external changes (such as from an MCP process)
// and broadcasts updated snapshots to connected SSE subscribers.
func (s *Service) WatchStateFile(ctx context.Context, interval time.Duration) {
	if s.stateFile == "" {
		return
	}
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			previousError := s.stateErrorStringLocked()
			changed := s.reloadStateFileIfChangedLocked()
			changed = changed || previousError != s.stateErrorStringLocked()
			var snap Snapshot
			if changed {
				snap = s.snapshotLocked()
			}
			s.mu.Unlock()
			if changed {
				s.broadcast(snap)
			}
		}
	}
}

// writeAtomicState uses a private temporary file in the same directory, so an
// interrupted or competing writer cannot reuse another writer's temporary path.
func writeAtomicState(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return nil
}
