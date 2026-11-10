package prdash_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/tenntenn/prdash/prdash"
)

func TestReviewWorktreeSelection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, assigned, selectedBranch, selectedPath string
		other                                        bool
		wantPath                                     string
		wantErr                                      bool
	}{
		{"explicit ID wins over matching branch", "sample-selected", "sample-branch", "/tmp/sample-selected", true, "/tmp/sample-selected", false},
		{"explicit detached worktree", "sample-selected", "detached", "/tmp/sample-selected", true, "/tmp/sample-selected", false},
		{"missing explicit ID", "sample-missing", "sample-branch", "/tmp/sample-selected", true, "", true},
		{"ambiguous branch", "", "sample-branch", "/tmp/sample-selected", true, "", true},
		{"unique branch fallback", "", "sample-branch", "/tmp/sample-selected", false, "/tmp/sample-selected", false},
		{"missing path", "sample-selected", "sample-branch", "", true, "", true},
		{"relative path", "sample-selected", "sample-branch", "sample-relative", true, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reviewer := &recordingWorktreeReviewer{}
			svc := prdash.NewService(prdash.Options{Repository: "example/sample", Reviewer: reviewer})
			if tc.other {
				if _, err := svc.UpsertWorktree(prdash.Worktree{ID: "sample-other", Branch: "sample-branch", Path: "/tmp/sample-other", HeadSHA: "sample-other-sha"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := svc.UpsertWorktree(prdash.Worktree{ID: "sample-selected", Branch: tc.selectedBranch, Path: tc.selectedPath, HeadSHA: "sample-selected-sha"}); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.UpsertPR(prdash.PullRequest{Number: 42, Title: "sample-pr", Branch: "sample-branch", WorktreeID: tc.assigned}); err != nil {
				t.Fatal(err)
			}
			_, reviewErr := svc.RequestReview(t.Context(), 42, "")
			_, commentErr := svc.PostSBNNComment(t.Context(), 42, "main.go", 1, "sample-comment")
			if tc.wantErr {
				if !errors.Is(reviewErr, prdash.ErrReviewWorktreeUnavailable) || !errors.Is(commentErr, prdash.ErrReviewWorktreeUnavailable) {
					t.Fatalf("review error=%v comment error=%v", reviewErr, commentErr)
				}
				if reviewer.reviewCalls != 0 || reviewer.commentCalls != 0 {
					t.Fatal("dispatched despite unavailable worktree")
				}
				return
			}
			if reviewErr != nil || commentErr != nil {
				t.Fatalf("review error=%v comment error=%v", reviewErr, commentErr)
			}
			if reviewer.review.WorktreePath != tc.wantPath || reviewer.comment.WorktreePath != tc.wantPath || reviewer.review.HeadSHA != "sample-selected-sha" || reviewer.comment.HeadSHA != "sample-selected-sha" {
				t.Fatalf("wrong execution target: review=%+v comment=%+v", reviewer.review, reviewer.comment)
			}
		})
	}
}

type recordingWorktreeReviewer struct {
	prdash.StubSBNNClient
	review                    prdash.SBNNReviewRequest
	comment                   prdash.SBNNCommentRequest
	reviewCalls, commentCalls int
}

func (r *recordingWorktreeReviewer) RunReview(ctx context.Context, req prdash.SBNNReviewRequest) (prdash.SBNNReviewResult, error) {
	r.review = req
	r.reviewCalls++
	return r.StubSBNNClient.RunReview(ctx, req)
}

func (r *recordingWorktreeReviewer) SubmitComment(ctx context.Context, req prdash.SBNNCommentRequest) (prdash.SBNNCommentResult, error) {
	r.comment = req
	r.commentCalls++
	return r.StubSBNNClient.SubmitComment(ctx, req)
}

func prepareSBNNRepository(t *testing.T, binDir string) {
	t.Helper()
	// Use a real local diff: delivery failures must no longer be ignored.
	for _, args := range [][]string{
		{"init", "--initial-branch=main"},
		{"-c", "user.name=sample", "-c", "user.email=sample@example.com", "commit", "--allow-empty", "-m", "sample base"},
		{"switch", "-c", "feat/sample"},
	} {
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", binDir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("prepare repository: %v: %s", err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(binDir, "main.go"), []byte("package sample\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "--", filepath.Join(binDir, "main.go")},
		{"-c", "user.name=sample", "-c", "user.email=sample@example.com", "commit", "-m", "sample change"},
	} {
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", binDir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("prepare diff: %v: %s", err, out)
		}
	}

}
