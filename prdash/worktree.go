package prdash

import (
	"context"
"errors"
	"fmt"
	"os"
"os/exec"
	"path/filepath"
	"strings"
)

// InspectWorktree refreshes a worktree from Git without changing its files.
// Failed inspection never establishes cleanliness, even for a previously clean entry.
func InspectWorktree(ctx context.Context, wt Worktree) Worktree {
	wt.Clean = false
	wt.Status = "unknown"
	wt.StatusError = ""
	if wt.Prunable {
		wt.Status = "prunable"
		return wt
	}
	if !filepath.IsAbs(wt.Path) {
		wt.Status = "error"
		wt.StatusError = "worktree path must be absolute"
		return wt
	}
	info, err := os.Stat(wt.Path)
	if err != nil {
		wt.Status = "error"
		if os.IsNotExist(err) {
			wt.Status = "missing"
		}
		wt.StatusError = err.Error()
		return wt
	}
	if !info.IsDir() {
		wt.Status = "error"
		wt.StatusError = "worktree path is not a directory"
		return wt
	}
	root, rootErr := runGit(ctx, wt.Path, "rev-parse", "--show-toplevel")
	if rootErr != nil {
		wt.Status = "error"
		wt.StatusError = rootErr.Error()
		return wt
	}
	actualPath, pathErr := filepath.EvalSymlinks(wt.Path)
	actualRoot, resolveErr := filepath.EvalSymlinks(root)
	if pathErr != nil || resolveErr != nil || actualPath != actualRoot {
		wt.Status = "error"
		wt.StatusError = "path is not the Git worktree root"
		return wt
	}
	head, headErr := runGit(ctx, wt.Path, "rev-parse", "--verify", "HEAD^{commit}")
	if headErr != nil {
		wt.Status = "error"
		wt.StatusError = headErr.Error()
		return wt
	}
	wt.HeadSHA = head
	branch, branchErr := runGit(ctx, wt.Path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if branchErr != nil {
		exitErr, ok := errors.AsType[*exec.ExitError](branchErr)
		if !ok || exitErr.ExitCode() != 1 {
			wt.Status = "error"
			wt.StatusError = branchErr.Error()
			return wt
		}
	}
	wt.Detached = branchErr != nil
	wt.Branch = branch
	if wt.Detached {
		wt.Branch = "HEAD"
	}
	status, statusErr := runGit(ctx, wt.Path, "status", "--porcelain")
	if statusErr != nil {
		wt.Status = "error"
		wt.StatusError = statusErr.Error()
		return wt
	}
	wt.Clean = strings.TrimSpace(status) == ""
	wt.Status = "dirty"
	if wt.Clean {
		wt.Status = "clean"
	}
	return wt
}

// ValidatePRWorktree rechecks the execution directory and its current HEAD.
// Agents should call this immediately before applying queued changes.
func (s *Service) ValidatePRWorktree(ctx context.Context, prNumber int) (Worktree, error) {
	s.mu.RLock()
	pr, _ := s.findPRLocked(prNumber)
	if pr == nil {
		s.mu.RUnlock()
		return Worktree{}, fmt.Errorf("%w: #%d", ErrPRNotFound, prNumber)
	}
	snapshot := clonePR(*pr)
	if snapshot.HeadSHA == "" {
		s.mu.RUnlock()
		return Worktree{},fmt.Errorf("%w: PR #%d has no target commit; sync or register its full head SHA",ErrReviewWorktreeUnavailable,prNumber)
	}
	// Reinspection can recover a previously missing or failed worktree.
	candidates := append([]Worktree(nil), s.worktrees...)
	for i := range candidates {
		candidates[i].Status = ""
		candidates[i].Prunable = false
		if snapshot.WorktreeID != "" {
			candidates[i].HeadSHA = ""
		}
	}
	selected, selectErr := selectReviewWorktree(&snapshot, candidates)
	if selectErr != nil {
		s.mu.RUnlock()
		return Worktree{}, selectErr
	}
	if selected == nil {
		s.mu.RUnlock()
		return Worktree{}, fmt.Errorf("%w: PR #%d has no execution worktree", ErrReviewWorktreeUnavailable, prNumber)
	}
	wt := *selected
	s.mu.RUnlock()
	wt = InspectWorktree(ctx, wt)
	if wt.Status != "clean" && wt.Status != "dirty" {
		return wt, fmt.Errorf("%w: %s: %s", ErrReviewWorktreeUnavailable, wt.Path, wt.StatusError)
	}
	if snapshot.HeadSHA != "" && snapshot.HeadSHA != wt.HeadSHA {
		return wt, fmt.Errorf("%w: expected commit %s, found %s", ErrReviewWorktreeUnavailable, snapshot.HeadSHA, wt.HeadSHA)
	}
	s.mu.RLock()
	current, _ := s.findPRLocked(prNumber)
	changed := current == nil || current.WorktreeID != snapshot.WorktreeID || current.HeadSHA != snapshot.HeadSHA || current.Branch != snapshot.Branch
	if !changed {
		for _, entry := range s.worktrees {
			if entry.ID == wt.ID && entry.Path != wt.Path {
				changed = true
				break
			}
		}
	}
	s.mu.RUnlock()
	if changed {
		return wt, fmt.Errorf("%w: PR target changed during validation", ErrReviewWorktreeUnavailable)
	}
	return wt, nil
}
