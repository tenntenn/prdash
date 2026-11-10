package prdash

import (
	"context"
	"encoding/json"
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
	payload := persistedState{
		Revision:          s.stateRev + 1,
		InstructionClaims: s.instructionClaims,
		Repository:        s.repo,
		ModelConfig:       s.modelConfig,
		Controller:        s.controller,
		Worktrees:         s.worktrees,
		Sessions:          s.sessions,
		PullRequests:      s.prs,
		Notifications:     s.notifications,
		Seq:               s.seq.Load(),
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.stateFile), 0o755); err != nil {
		return err
	}
	tmp := s.stateFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.stateFile); err != nil {
		return err
	}
	s.stateRev++
	return nil
}

func (s *Service) reloadStateFileIfChangedLocked() bool {
	if s.stateFile == "" {
		return false
	}
	data, err := os.ReadFile(s.stateFile)
	if err != nil {
		return false
	}
	var payload persistedState
	if err := json.Unmarshal(data, &payload); err != nil {
		return false
	}
	if payload.Revision <= s.stateRev {
		return false
	}
	s.instructionClaims = payload.InstructionClaims
	s.stateRev = payload.Revision
	s.repo = payload.Repository
	s.modelConfig = payload.ModelConfig
	s.controller = payload.Controller
	s.worktrees = payload.Worktrees
	s.sessions = payload.Sessions
	s.prs = payload.PullRequests
	s.notifications = payload.Notifications
	if payload.Seq > s.seq.Load() {
		s.seq.Store(payload.Seq)
	}
	return true
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
			changed := s.reloadStateFileIfChangedLocked()
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
