package prdash

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

var (
	ErrStatePersistence = errors.New("state persistence failed")
	ErrStateCorrupt     = errors.New("state file is corrupt")
	ErrStateConflict    = errors.New("state revision conflict")
)

type stateTransaction struct {
	lock   *flock.Flock
	before persistedState
	hash   [sha256.Size]byte
}

// StatePath returns the selected canonical state file, or empty for memory only.
func (s *Service) StatePath() string { s.mu.RLock(); defer s.mu.RUnlock(); return s.stateFile }

// StateError reports the most recent state load or persistence failure.
func (s *Service) StateError() error { s.mu.RLock(); defer s.mu.RUnlock(); return s.stateErr }
func (s *Service) stateErrorStringLocked() string {
	if s.stateErr != nil {
		return s.stateErr.Error()
	}
	return ""
}

func (s *Service) initializeStateFile() {
	absolute, err := filepath.Abs(s.stateFile)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(absolute), 0o700)
	}
	if err == nil {
		parent, parentErr := filepath.EvalSymlinks(filepath.Dir(absolute))
		err = parentErr
		absolute = filepath.Join(parent, filepath.Base(absolute))
	}
	if err == nil {
		resolved, resolveErr := filepath.EvalSymlinks(absolute)
		if resolveErr == nil {
			absolute = resolved
		} else if !errors.Is(resolveErr, os.ErrNotExist) {
			err = resolveErr
		}
	}
	s.stateFile = absolute
	if err != nil {
		s.stateErr = fmt.Errorf("%w: %w", ErrStatePersistence, err)
		return
	}
	if err = s.beginStateMutation(); err != nil {
		return
	}
	if s.stateRev == 0 {
		_, _ = s.commitStateMutation()
	} else {
		s.abortStateMutation()
	}
}

// beginStateMutation holds both the local mutex and a stable sidecar OS lock.
// Call only after external work; every exit must commit or abort the transaction.
func (s *Service) beginStateMutation() error {
	s.mu.Lock()
	var lock *flock.Flock
	if s.stateFile != "" {
		lock = flock.New(s.stateFile + ".lock")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		locked, err := lock.TryLockContext(ctx, 10*time.Millisecond)
		cancel()
		if err == nil && !locked {
			err = errors.New("timed out waiting for state lock")
		}
		if err != nil {
			s.stateErr = fmt.Errorf("%w: lock: %w", ErrStatePersistence, err)
			_ = lock.Close()
			failure := s.stateErr
			s.mu.Unlock()
			return failure
		}
		if _, err := s.loadStateFileLocked(); err != nil {
			s.stateErr = err
			_ = lock.Close()
			s.mu.Unlock()
			return err
		}
	}
	s.transaction = &stateTransaction{lock: lock, before: s.captureStateLocked(), hash: s.stateHash}
	s.stateErr = nil
	return nil
}

func (s *Service) captureStateLocked() persistedState {
	snap := s.snapshotLocked()
	return persistedState{Revision: s.stateRev, InstructionClaims: maps.Clone(s.instructionClaims), Repository: snap.Repository, ModelConfig: snap.ModelConfig, Controller: snap.ControllerSession, Worktrees: snap.Worktrees, Sessions: snap.Sessions, PullRequests: snap.PullRequests, Notifications: snap.Notifications, Seq: s.seq.Load()}
}

func (s *Service) restoreStateLocked(state persistedState) {
	s.stateRev = state.Revision
	s.instructionClaims = state.InstructionClaims
	s.repo = state.Repository
	s.modelConfig = state.ModelConfig
	s.controller = state.Controller
	s.worktrees = state.Worktrees
	s.sessions = state.Sessions
	s.prs = state.PullRequests
	s.notifications = state.Notifications
	s.seq.Store(state.Seq)
	s.dirty = false
}

func (s *Service) abortStateMutation() {
	tx := s.transaction
	s.restoreStateLocked(tx.before)
	s.stateHash = tx.hash
	s.transaction = nil
	if tx.lock != nil {
		_ = tx.lock.Close()
	}
	s.mu.Unlock()
}

func (s *Service) commitStateMutation() (Snapshot, error) {
	if err := s.saveStateFileLocked(); err != nil {
		s.stateErr = fmt.Errorf("%w: %w", ErrStatePersistence, err)
		failure := s.stateErr
		s.abortStateMutation()
		return Snapshot{}, failure
	}
	s.dirty = false
	s.stateErr = nil
	snap := s.snapshotLocked()
	tx := s.transaction
	s.transaction = nil
	var unlockErr error
	if tx.lock != nil {
		unlockErr = tx.lock.Close()
	}
	if unlockErr != nil {
		s.stateErr = fmt.Errorf("%w: unlock: %w", ErrStatePersistence, unlockErr)
	}
	failure := s.stateErr
	s.mu.Unlock()
	return snap, failure
}
