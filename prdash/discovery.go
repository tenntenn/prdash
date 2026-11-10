package prdash

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// DetectRepositoryInfo inspects the git repository at dir (or the current working directory if empty)
// and returns its owner/name, default branch, and local root path.
func DetectRepositoryInfo(ctx context.Context, dir string) RepositoryInfo {
	if strings.TrimSpace(dir) == "" {
		if wd, err := os.Getwd(); err == nil {
			dir = wd
		}
	}

	root := strings.TrimSpace(dir)
	if out, err := runGit(ctx, dir, "rev-parse", "--show-toplevel"); err == nil && out != "" {
		root = out
	}

	defaultBranch := "main"
	if out, err := runGit(ctx, root, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if _, branch, ok := strings.Cut(out, "/"); ok && branch != "" {
			defaultBranch = branch
		}
	}

	var repo RepositoryInfo
	if out, err := runGit(ctx, root, "remote", "get-url", "origin"); err == nil {
		repo = ParseGitRemoteURL(out)
	}
	if repo.Name == "" && root != "" {
		base := filepath.Base(root)
		repo.Name = base
		repo.FullName = base
	}
	repo.DefaultBranch = defaultBranch
	repo.LocalPath = root
	return repo
}

// ParseGitRemoteURL extracts owner and repository name from an HTTPS or SSH git remote URL.
func ParseGitRemoteURL(rawURL string) RepositoryInfo {
	u := strings.TrimSpace(rawURL)
	u = strings.TrimSuffix(u, ".git")
	u = strings.TrimSuffix(u, "/")

	var pathPart string
	switch {
	case strings.Contains(u, "://"):
		if _, afterScheme, ok := strings.Cut(u, "://"); ok {
			if _, afterHost, okHost := strings.Cut(afterScheme, "/"); okHost {
				pathPart = afterHost
			}
		}
	case strings.Contains(u, ":"):
		if _, afterColon, ok := strings.Cut(u, ":"); ok {
			pathPart = afterColon
		}
	default:
		pathPart = u
	}

	parts := strings.Split(strings.Trim(pathPart, "/"), "/")
	if len(parts) >= 2 {
		owner := parts[len(parts)-2]
		name := parts[len(parts)-1]
		return RepositoryInfo{
			Owner:         owner,
			Name:          name,
			FullName:      owner + "/" + name,
			DefaultBranch: "main",
		}
	}
	return ParseRepositoryInfo(pathPart)
}

// DiscoverWorktrees runs `git worktree list --porcelain` in dir and inspects working tree cleanliness.
func DiscoverWorktrees(ctx context.Context, dir string) ([]Worktree, error) {
	out, err := runGit(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("git worktree list: %w", err)
	}
	worktrees := ParseGitWorktreePorcelain(out)
	for i := range worktrees {
		if statusOut, statusErr := runGit(ctx, worktrees[i].Path, "status", "--porcelain"); statusErr == nil {
			worktrees[i].Clean = strings.TrimSpace(statusOut) == ""
		}
	}
	return worktrees, nil
}

// ParseGitWorktreePorcelain parses the output of `git worktree list --porcelain`.
func ParseGitWorktreePorcelain(porcelain string) []Worktree {
	blocks := strings.Split(strings.TrimSpace(porcelain), "\n\n")
	worktrees := make([]Worktree, 0, len(blocks))
	usedIDs := make(map[string]int)

	for _, block := range blocks {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var (
			wtPath string
			head   string
			branch string
			bare   bool
		)
		for line := range strings.SplitSeq(block, "\n") {
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "worktree "):
				wtPath = strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
			case strings.HasPrefix(line, "HEAD "):
				head = strings.TrimSpace(strings.TrimPrefix(line, "HEAD "))
				if len(head) > 7 {
					head = head[:7]
				}
			case strings.HasPrefix(line, "branch "):
				ref := strings.TrimSpace(strings.TrimPrefix(line, "branch "))
				branch = strings.TrimPrefix(ref, "refs/heads/")
			case line == "detached":
				if branch == "" {
					branch = "HEAD"
				}
			case line == "bare":
				bare = true
			}
		}
		if bare || wtPath == "" {
			continue
		}
		if branch == "" {
			branch = "main"
		}
		baseID := "wt-" + sanitizeID(filepath.Base(wtPath))
		usedIDs[baseID]++
		id := baseID
		if usedIDs[baseID] > 1 {
			id = fmt.Sprintf("%s-%d", baseID, usedIDs[baseID])
		}
		worktrees = append(worktrees, Worktree{
			ID:      id,
			Path:    wtPath,
			Branch:  branch,
			HeadSHA: head,
			Clean:   true,
		})
	}
	return worktrees
}

type ghPRItem struct {
	HeadRefOid        string           `json:"headRefOid"`
	Number            int              `json:"number"`
	Title             string           `json:"title"`
	Body              string           `json:"body"`
	HeadRefName       string           `json:"headRefName"`
	BaseRefName       string           `json:"baseRefName"`
	Additions         int              `json:"additions"`
	Deletions         int              `json:"deletions"`
	ChangedFiles      int              `json:"changedFiles"`
	Mergeable         string           `json:"mergeable"`
	ReviewDecision    string           `json:"reviewDecision"`
	StatusCheckRollup []ghStatusRollup `json:"statusCheckRollup"`
	UpdatedAt         time.Time        `json:"updatedAt"`
}

type ghStatusRollup struct {
	State      string `json:"state"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// DiscoverPullRequests queries open pull requests from GitHub using the `gh` CLI.
func DiscoverPullRequests(ctx context.Context, repoFullName, dir string, defaultBranch string, worktrees []Worktree, modelCfg ModelPresetConfig) ([]PullRequest, error) {
	args := []string{
		"pr", "list",
		"--state", "open",
		"--limit", "100",
		"--json", "number,title,body,headRefName,headRefOid,baseRefName,additions,deletions,changedFiles,mergeable,reviewDecision,statusCheckRollup,updatedAt",
	}
	if strings.Contains(repoFullName, "/") {
		args = append(args, "--repo", repoFullName)
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %w", err)
	}
	return ParseGitHubPRListJSON(out, defaultBranch, worktrees, modelCfg)
}

// ParseGitHubPRListJSON converts `gh pr list --json ...` output into PullRequest structs,
// linking matching worktrees by branch and inferring stacked PR chains from base/head branch pairs.
func ParseGitHubPRListJSON(data []byte, defaultBranch string, worktrees []Worktree, modelCfg ModelPresetConfig) ([]PullRequest, error) {
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	var items []ghPRItem
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("parse gh pr list json: %w", err)
	}

	wtByBranch := make(map[string]Worktree, len(worktrees))
	for _, wt := range worktrees {
		if wt.Branch != "" {
			wtByBranch[wt.Branch] = wt
		}
	}

	prs := make([]PullRequest, 0, len(items))
	byHead := make(map[string]int, len(items))
	for _, item := range items {
		wt := wtByBranch[item.HeadRefName]
		updated := item.UpdatedAt
		if updated.IsZero() {
			updated = time.Now().UTC()
		}
		pr := PullRequest{
			HeadSHA:       item.HeadRefOid,
			ID:            fmt.Sprintf("pr-%d", item.Number),
			Number:        item.Number,
			Title:         item.Title,
			Description:   item.Body,
			Branch:        item.HeadRefName,
			BaseBranch:    item.BaseRefName,
			WorktreeID:    wt.ID,
			SessionID:     wt.SessionID,
			Status:        StatusWaitingForHuman,
			ModelTier:     TierBamboo,
			ResolvedModel: modelCfg.Resolve(TierBamboo),
			CIStatus:      mapGHCIStatus(item.StatusCheckRollup),
			HasConflicts:  strings.EqualFold(item.Mergeable, "CONFLICTING"),
			ReviewStatus:  mapGHReviewDecision(item.ReviewDecision),
			Additions:     item.Additions,
			Deletions:     item.Deletions,
			ChangedFiles:  item.ChangedFiles,
			Thread:        make([]ThreadMessage, 0),
			UpdatedAt:     updated,
		}
		byHead[pr.Branch] = len(prs)
		prs = append(prs, pr)
	}

	inferStackedChains(prs, byHead, defaultBranch)
	return prs, nil
}

func inferStackedChains(prs []PullRequest, byHead map[string]int, defaultBranch string) {
	hasChild := make(map[int]bool)
	for i := range prs {
		if parentIdx, ok := byHead[prs[i].BaseBranch]; ok && parentIdx != i {
			prs[i].ParentPRNumber = prs[parentIdx].Number
			hasChild[parentIdx] = true
		}
	}

	for i := range prs {
		if prs[i].ParentPRNumber == 0 && !hasChild[i] && (prs[i].BaseBranch == defaultBranch || prs[i].BaseBranch == "master" || prs[i].BaseBranch == "") {
			continue
		}
		if prs[i].ParentPRNumber == 0 && !hasChild[i] {
			continue
		}
		// Walk up to the root PR of this stack chain.
		rootIdx := i
		visited := make(map[int]bool)
		order := 1
		for prs[rootIdx].ParentPRNumber != 0 && !visited[rootIdx] {
			visited[rootIdx] = true
			parentIdx, ok := byHead[prs[rootIdx].BaseBranch]
			if !ok {
				break
			}
			rootIdx = parentIdx
			order++
		}
		prs[i].StackID = fmt.Sprintf("stack-pr-%d", prs[rootIdx].Number)
		prs[i].StackOrder = order
	}

	// Sort each stack chain and populate StackSize.
	stackCounts := make(map[string]int)
	for i := range prs {
		if prs[i].StackID != "" {
			stackCounts[prs[i].StackID]++
		}
	}
	for i := range prs {
		if prs[i].StackID != "" {
			prs[i].StackSize = stackCounts[prs[i].StackID]
		}
	}
	slices.SortStableFunc(prs, func(a, b PullRequest) int {
		if a.StackID != "" && a.StackID == b.StackID {
			return a.StackOrder - b.StackOrder
		}
		return b.Number - a.Number
	})
}

func mapGHCIStatus(checks []ghStatusRollup) string {
	if len(checks) == 0 {
		return "passing"
	}
	hasRunning := false
	for _, c := range checks {
		state := strings.ToUpper(strings.TrimSpace(c.Conclusion))
		if state == "" {
			state = strings.ToUpper(strings.TrimSpace(c.State))
		}
		status := strings.ToUpper(strings.TrimSpace(c.Status))
		switch state {
		case "FAILURE", "ERROR", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED":
			return "failing"
		case "PENDING", "EXPECTED":
			hasRunning = true
		}
		if status == "IN_PROGRESS" || status == "QUEUED" || status == "PENDING" {
			hasRunning = true
		}
	}
	if hasRunning {
		return "running"
	}
	return "passing"
}

func mapGHReviewDecision(decision string) string {
	switch strings.ToUpper(strings.TrimSpace(decision)) {
	case "APPROVED":
		return "approved"
	case "CHANGES_REQUESTED":
		return "changes_requested"
	default:
		return "review_pending"
	}
}

func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	res := strings.Trim(b.String(), "-")
	if res == "" {
		return "default"
	}
	return res
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if strings.TrimSpace(dir) != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
