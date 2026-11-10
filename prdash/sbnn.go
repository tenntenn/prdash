package prdash

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// SBNNCommentRequest describes a pull request comment dispatched through the local sbnn review tool.
type SBNNCommentRequest struct {
	WorktreePath  string          `json:"worktree_path,omitempty"`
	HeadSHA       string          `json:"head_sha,omitempty"`
	BaseBranch    string          `json:"base_branch,omitempty"`
	AppliedConfig EffectiveConfig `json:"applied_config"`
	Repository    string          `json:"repository"`
	PRNumber      int             `json:"pr_number"`
	Branch        string          `json:"branch"`
	Worktree      string          `json:"worktree"`
	FilePath      string          `json:"file_path,omitempty"`
	Line          int             `json:"line,omitempty"`
	Body          string          `json:"body"`
}

// SBNNCommentResult contains the formatted comment record produced by sbnn.
type SBNNCommentResult struct {
	CommentID   string    `json:"comment_id"`
	CommandLine string    `json:"command_line"`
	Formatted   string    `json:"formatted"`
	CreatedAt   time.Time `json:"created_at"`
}

// SBNNReviewRequest describes an automated review request sent to sbnn.
type SBNNReviewRequest struct {
	WorktreePath  string          `json:"worktree_path,omitempty"`
	HeadSHA       string          `json:"head_sha,omitempty"`
	AppliedConfig EffectiveConfig `json:"applied_config"`
	Repository    string          `json:"repository"`
	PRNumber      int             `json:"pr_number"`
	Branch        string          `json:"branch"`
	BaseBranch    string          `json:"base_branch"`
	Worktree      string          `json:"worktree"`
	Focus         string          `json:"focus,omitempty"`
}

// SBNNReviewResult contains the structured findings returned by sbnn.
type SBNNReviewResult struct {
	CommandLine string    `json:"command_line"`
	Summary     string    `json:"summary"`
	Findings    []string  `json:"findings"`
	CreatedAt   time.Time `json:"created_at"`
}

// SBNNReviewer defines the integration boundary for the local sbnn review tool.
type SBNNReviewer interface {
	SubmitComment(ctx context.Context, req SBNNCommentRequest) (SBNNCommentResult, error)
	RunReview(ctx context.Context, req SBNNReviewRequest) (SBNNReviewResult, error)
}

// CLISBNNClient invokes the real `sbnn` CLI binary on PATH.
type CLISBNNClient struct {
	Bin string
	Dir string
}

// NewCLISBNNClient returns a CLISBNNClient configured with the given binary name and working directory.
func NewCLISBNNClient(bin, dir string) *CLISBNNClient {
	if strings.TrimSpace(bin) == "" {
		bin = "sbnn"
	}
	return &CLISBNNClient{
		Bin: bin,
		Dir: dir,
	}
}

// SubmitComment runs `sbnn comment <file>:<line> -m <body> --author prdash --target pr-<number>`.
func (c *CLISBNNClient) SubmitComment(ctx context.Context, req SBNNCommentRequest) (SBNNCommentResult, error) {
	body := strings.TrimSpace(req.Body)
	if body == "" {
		return SBNNCommentResult{}, errors.New("sbnn comment body is required")
	}

	target := fmt.Sprintf("pr-%d", req.PRNumber)
	workDir, workErr := c.reviewWorkDir(ctx, req.WorktreePath)
	if workErr != nil {
		return SBNNCommentResult{}, workErr
	}
	diffText := c.ensurePRDiffSent(ctx, req.Repository, req.PRNumber, req.BaseBranch, req.Branch, target, workDir)

	filePath := strings.TrimSpace(req.FilePath)
	if filePath == "" {
		filePath = firstDiffFilePath(diffText)
		if filePath == "" {
			filePath = "PR"
		}
	}
	line := req.Line
	if line <= 0 {
		line = 1
	}
	anchor := fmt.Sprintf("%s:%d", filePath, line)

	args := []string{"comment", anchor, "-m", body, "--author", "prdash", "--target", target}
	cmdLine := c.Bin + " " + strings.Join(args, " ")

	cmd := exec.CommandContext(ctx, c.Bin, args...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return SBNNCommentResult{}, fmt.Errorf("%s: %w (%s)", cmdLine, err, strings.TrimSpace(string(out)))
	}

	now := time.Now().UTC()
	return SBNNCommentResult{
		CommentID:   fmt.Sprintf("sbnn-cmt-%d-%d", req.PRNumber, now.UnixNano()%100000),
		CommandLine: cmdLine,
		Formatted:   fmt.Sprintf("[sbnn %s] %s", anchor, body),
		CreatedAt:   now,
	}, nil
}

func (c *CLISBNNClient) ensurePRDiffSent(ctx context.Context, repo string, prNumber int, base, head, target, workDir string) string {
	if strings.TrimSpace(base) == "" {
		base = "main"
	}
	if strings.TrimSpace(head) == "" {
		head = "HEAD"
	}
	diffOut, diffErr := runGit(ctx, workDir, "diff", base+"..."+head)
	if (diffErr != nil || diffOut == "") && prNumber > 0 {
		ghArgs := []string{"pr", "diff", strconv.Itoa(prNumber)}
		if strings.TrimSpace(repo) != "" && strings.Contains(repo, "/") {
			ghArgs = append(ghArgs, "--repo", strings.TrimSpace(repo))
		}
		ghCmd := exec.CommandContext(ctx, "gh", ghArgs...)
		if workDir != "" {
			ghCmd.Dir = workDir
		}
		if out, err := ghCmd.Output(); err == nil {
			diffOut = string(out)
		}
	}
	if strings.TrimSpace(diffOut) != "" {
		sendArgs := []string{
			"--target", target,
			"--title", fmt.Sprintf("PR #%d (%s)", prNumber, head),
			"--label", "pr=" + strconv.Itoa(prNumber),
			"--no-open",
			"--json",
		}
		sendCmd := exec.CommandContext(ctx, c.Bin, sendArgs...)
		if workDir != "" {
			sendCmd.Dir = workDir
		}
		sendCmd.Stdin = strings.NewReader(diffOut)
		_ = sendCmd.Run()
	}
	return diffOut
}

func firstDiffFilePath(diffText string) string {
	for line := range strings.SplitSeq(diffText, "\n") {
		if after, ok := strings.CutPrefix(line, "+++ b/"); ok {
			p := strings.TrimSpace(after)
			if p != "" && p != "/dev/null" {
				return p
			}
		}
	}
	return ""
}

type sbnnCommentEntry struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Author    string `json:"author"`
	StartLine int    `json:"startLine"`
	EndLine   int    `json:"endLine"`
	Body      string `json:"body"`
	Resolved  bool   `json:"resolved"`
}

// RunReview sends the pull request diff to `sbnn` and reads open comments from `sbnn comments --format json`.
func (c *CLISBNNClient) RunReview(ctx context.Context, req SBNNReviewRequest) (SBNNReviewResult, error) {
	target := fmt.Sprintf("pr-%d", req.PRNumber)
	workDir, workErr := c.reviewWorkDir(ctx, req.WorktreePath)
	if workErr != nil {
		return SBNNReviewResult{}, workErr
	}
	_ = c.ensurePRDiffSent(ctx, req.Repository, req.PRNumber, req.BaseBranch, req.Branch, target, workDir)

	commentsArgs := []string{"comments", "--target", target, "--format", "json"}
	cmdLine := c.Bin + " " + strings.Join(commentsArgs, " ")
	cmd := exec.CommandContext(ctx, c.Bin, commentsArgs...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	// Note: `sbnn comments` exits with status 1 when unresolved comments exist, and 0 when clean.
	if err != nil && stdout.Len() == 0 {
		return SBNNReviewResult{}, fmt.Errorf("%s: %w (%s)", cmdLine, err, strings.TrimSpace(stderr.String()))
	}

	findings := make([]string, 0)
	if raw := bytes.TrimSpace(stdout.Bytes()); len(raw) > 0 && raw[0] == '[' {
		var entries []sbnnCommentEntry
		if jsonErr := json.Unmarshal(raw, &entries); jsonErr == nil {
			for _, e := range entries {
				if e.Resolved {
					continue
				}
				findings = append(findings, fmt.Sprintf("%s:%d: %s", e.Path, e.StartLine, e.Body))
			}
		}
	}

	summary := fmt.Sprintf("sbnn review synced for PR #%d (target %q, %d open comment(s)).", req.PRNumber, target, len(findings))
	if len(findings) > 0 {
		summary += " " + strings.Join(findings, " | ")
	}
	return SBNNReviewResult{
		CommandLine: cmdLine,
		Summary:     summary,
		Findings:    findings,
		CreatedAt:   time.Now().UTC(),
	}, nil
}

// StubSBNNClient provides a deterministic in-memory implementation of SBNNReviewer for unit tests.
type StubSBNNClient struct{}

// NewStubSBNNClient returns a new StubSBNNClient instance.
func NewStubSBNNClient() *StubSBNNClient {
	return &StubSBNNClient{}
}

// SubmitComment records an in-memory sbnn comment for unit testing.
func (s *StubSBNNClient) SubmitComment(_ context.Context, req SBNNCommentRequest) (SBNNCommentResult, error) {
	body := strings.TrimSpace(req.Body)
	if body == "" {
		return SBNNCommentResult{}, errors.New("sbnn comment body is required")
	}

	location := "PR-level"
	cmdArgs := fmt.Sprintf("sbnn comment --target pr-%d", req.PRNumber)
	if strings.TrimSpace(req.FilePath) != "" {
		if req.Line > 0 {
			location = fmt.Sprintf("%s:%d", req.FilePath, req.Line)
		} else {
			location = req.FilePath
		}
		cmdArgs += " " + location
	}
	cmdArgs += fmt.Sprintf(" -m %q", body)

	now := time.Now().UTC()
	return SBNNCommentResult{
		CommentID:   fmt.Sprintf("sbnn-cmt-%d-%d", req.PRNumber, now.UnixNano()%100000),
		CommandLine: cmdArgs,
		Formatted:   fmt.Sprintf("[sbnn %s] %s", location, body),
		CreatedAt:   now,
	}, nil
}

// RunReview returns deterministic review findings for unit testing.
func (s *StubSBNNClient) RunReview(_ context.Context, req SBNNReviewRequest) (SBNNReviewResult, error) {
	focus := strings.TrimSpace(req.Focus)
	cmdArgs := fmt.Sprintf("sbnn comments --target pr-%d --format json", req.PRNumber)
	findings := []string{
		fmt.Sprintf("Checked diff between %s and %s in worktree %s.", req.BaseBranch, req.Branch, req.Worktree),
	}
	if focus != "" {
		findings = append(findings, fmt.Sprintf("Focus area: %s.", focus))
	}
	summary := fmt.Sprintf("sbnn review completed for PR #%d (%d finding(s)): %s", req.PRNumber, len(findings), strings.Join(findings, " "))
	return SBNNReviewResult{
		CommandLine: cmdArgs,
		Summary:     summary,
		Findings:    findings,
		CreatedAt:   time.Now().UTC(),
	}, nil
}

func (c *CLISBNNClient) reviewWorkDir(ctx context.Context, path string) (string, error) {
	if path == "" {
		return c.Dir, nil
	}
	wt := InspectWorktree(ctx, Worktree{Path: path})
	if wt.Status != "clean" && wt.Status != "dirty" {
		return "", fmt.Errorf("%w: %s: %s", ErrReviewWorktreeUnavailable, path, wt.StatusError)
	}
	return path, nil
}
