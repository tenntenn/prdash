package prdash

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// AssignPR changes only a PR's execution worktree and destination session.
// Both destinations must already be registered. A session's own worktree is
// independent from the PR's execution worktree and is not changed here.
func (s *Service) AssignPR(number int, worktreeID, sessionID string) (PullRequest, error) {
	worktreeID, sessionID = strings.TrimSpace(worktreeID), strings.TrimSpace(sessionID)
	if worktreeID == "" || sessionID == "" {
		return PullRequest{}, errors.New("worktree and session IDs are required")
	}
	if err := s.beginStateMutation(); err != nil {
		return PullRequest{}, err
	}
	s.reloadStateFileIfChangedLocked()
	worktreeFound := false
	for _, wt := range s.worktrees {
		if wt.ID == worktreeID {
			worktreeFound = true
			break
		}
	}
	if !worktreeFound {
		s.abortStateMutation()
		return PullRequest{}, fmt.Errorf("worktree %q is not registered", worktreeID)
	}
	sessionFound := false
	for _, sess := range s.sessions {
		if sess.ID == sessionID {
			sessionFound = true
			break
		}
	}
	if !sessionFound {
		s.abortStateMutation()
		return PullRequest{}, fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
	}
	index := -1
	for i := range s.prs {
		if s.prs[i].Number == number {
			index = i
			break
		}
	}
	if index < 0 {
		s.abortStateMutation()
		return PullRequest{}, fmt.Errorf("%w: #%d", ErrPRNotFound, number)
	}
	pr := &s.prs[index]
	pr.WorktreeID = worktreeID
	pr.SessionID = sessionID
	pr.UpdatedAt = time.Now().UTC()
	s.dirty = true
	result := clonePR(*pr)
	snap, err := s.commitStateMutation()
	if err != nil {
		return PullRequest{}, err
	}
	s.broadcast(snap)
	return result, nil
}
