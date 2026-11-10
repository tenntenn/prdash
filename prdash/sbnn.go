package prdash

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SBNNCommentRequest describes a pull request comment dispatched through the local sbnn review tool.
type SBNNCommentRequest struct {
	Repository string `json:"repository"`
	PRNumber   int    `json:"pr_number"`
	Branch     string `json:"branch"`
	Worktree   string `json:"worktree"`
	FilePath   string `json:"file_path,omitempty"`
	Line       int    `json:"line,omitempty"`
	Body       string `json:"body"`
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
	Repository string `json:"repository"`
	PRNumber   int    `json:"pr_number"`
	Branch     string `json:"branch"`
	BaseBranch string `json:"base_branch"`
	Worktree   string `json:"worktree"`
	Focus      string `json:"focus,omitempty"`
}

// SBNNReviewResult contains the structured findings returned by sbnn.
type SBNNReviewResult struct {
	CommandLine string    `json:"command_line"`
	Summary     string    `json:"summary"`
	Findings    []string  `json:"findings"`
	CreatedAt   time.Time `json:"created_at"`
}

// SBNNReviewer defines the integration boundary for the local sbnn AI review tool.
// Replace StubSBNNClient with a CLI/process-backed implementation to invoke a live sbnn binary.
type SBNNReviewer interface {
	SubmitComment(ctx context.Context, req SBNNCommentRequest) (SBNNCommentResult, error)
	RunReview(ctx context.Context, req SBNNReviewRequest) (SBNNReviewResult, error)
}

// StubSBNNClient provides a deterministic local stub for the sbnn review tool.
type StubSBNNClient struct{}

// NewStubSBNNClient returns a new StubSBNNClient instance.
func NewStubSBNNClient() *StubSBNNClient {
	return &StubSBNNClient{}
}

// SubmitComment simulates invoking `sbnn comment` on a target pull request and file location.
func (s *StubSBNNClient) SubmitComment(_ context.Context, req SBNNCommentRequest) (SBNNCommentResult, error) {
	body := strings.TrimSpace(req.Body)
	if body == "" {
		return SBNNCommentResult{}, errors.New("sbnn comment body is required")
	}

	location := "PR-level"
	cmdArgs := fmt.Sprintf("sbnn comment --repo %s --pr %d", req.Repository, req.PRNumber)
	if strings.TrimSpace(req.FilePath) != "" {
		if req.Line > 0 {
			location = fmt.Sprintf("%s:%d", req.FilePath, req.Line)
			cmdArgs += fmt.Sprintf(" --file %s --line %d", req.FilePath, req.Line)
		} else {
			location = req.FilePath
			cmdArgs += fmt.Sprintf(" --file %s", req.FilePath)
		}
	}
	cmdArgs += fmt.Sprintf(" --body %q", body)

	now := time.Now().UTC()
	return SBNNCommentResult{
		CommentID:   fmt.Sprintf("sbnn-cmt-%d-%d", req.PRNumber, now.UnixNano()%100000),
		CommandLine: cmdArgs,
		Formatted:   fmt.Sprintf("[sbnn %s] %s", location, body),
		CreatedAt:   now,
	}, nil
}

// RunReview simulates invoking `sbnn review` against a pull request worktree.
func (s *StubSBNNClient) RunReview(_ context.Context, req SBNNReviewRequest) (SBNNReviewResult, error) {
	focus := strings.TrimSpace(req.Focus)
	cmdArgs := fmt.Sprintf("sbnn review --repo %s --pr %d --base %s --head %s", req.Repository, req.PRNumber, req.BaseBranch, req.Branch)
	if focus != "" {
		cmdArgs += fmt.Sprintf(" --focus %q", focus)
	}

	findings := []string{
		fmt.Sprintf("Verified diff boundary between %s and %s in worktree %s.", req.BaseBranch, req.Branch, req.Worktree),
		"Checked error wrapping, context propagation, and race-safety invariants.",
	}
	if focus != "" {
		findings = append(findings, fmt.Sprintf("Focused review note (%s): recommend adding an explicit edge-case assertion in unit tests.", focus))
	} else {
		findings = append(findings, "Suggested tightening input validation and ensuring deterministic error responses.")
	}

	summary := fmt.Sprintf("sbnn review completed for PR #%d (%d findings): %s", req.PRNumber, len(findings), strings.Join(findings, " "))
	return SBNNReviewResult{
		CommandLine: cmdArgs,
		Summary:     summary,
		Findings:    findings,
		CreatedAt:   time.Now().UTC(),
	}, nil
}
