package prdash_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tenntenn/prdash/prdash"
)

func TestWorktreePorcelainState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, lines, status string
		detached, prunable  bool
	}{
		{"branch", "branch refs/heads/sample", "unknown", false, false},
		{"detached", "detached", "unknown", true, false},
		{"prunable", "detached\nprunable gitdir file points to non-existent location", "prunable", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sha := strings.Repeat("a", 40)
			got := prdash.ParseGitWorktreePorcelain("worktree /tmp/sample\nHEAD " + sha + "\n" + tc.lines + "\n")
			if len(got) != 1 {
				t.Fatalf("worktrees=%+v", got)
			}
			wt := got[0]
			if wt.HeadSHA != sha || wt.Status != tc.status || wt.Clean || wt.Detached != tc.detached || wt.Prunable != tc.prunable {
				t.Fatalf("worktree=%+v", wt)
			}
		})
	}
}

func TestDiscoverWorktreeInspectionState(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name, status  string
		exists, clean bool
	}{
		{"sample-clean", "clean", true, true},
		{"sample-dirty", "dirty", true, false},
		{"sample-error", "error", true, false},
		{"sample-missing", "missing", false, false},
		{"sample-prunable", "prunable", false, false},
	}
	var porcelain strings.Builder
	for _, tc := range cases {
		path := filepath.Join(dir, tc.name)
		if tc.exists {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		fmt.Fprintf(&porcelain, "worktree %s\nHEAD %s\nbranch refs/heads/%s\n", path, strings.Repeat("a", 40), tc.name)
		if tc.status == "prunable" {
			porcelain.WriteString("prunable missing directory\n")
		}
		porcelain.WriteByte('\n')
	}
	script := `#!/bin/sh
case "$1" in
rev-parse)
 if [ "$2" = "--show-toplevel" ]; then echo "$PWD"; else echo aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; fi ;;
symbolic-ref) echo sample-branch ;;
worktree) cat "$(dirname "$0")/worktrees" ;;
status)
 case "$PWD" in
 */sample-dirty) echo ' M sample.go' ;;
 */sample-error) exit 2 ;;
 esac
 ;;
*) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "worktrees"), []byte(porcelain.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	got, err := prdash.DiscoverWorktrees(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(cases) {
		t.Fatalf("got %d worktrees", len(got))
	}
	for i, tc := range cases {
		wt := got[i]
		if wt.Status != tc.status || wt.Clean != tc.clean {
			t.Errorf("%s: %+v", tc.name, wt)
		}
		if (tc.status == "missing" || tc.status == "error") && wt.StatusError == "" {
			t.Errorf("%s: missing error detail", tc.name)
		}
	}
}

func TestReviewRejectsUnverifiedWorktree(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status  string
		wantErr bool
	}{
		{"unknown", true},
		{"missing", true},
		{"error", true},
		{"prunable", true},
		{"clean", false},
		{"dirty", false},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			t.Parallel()
			reviewer := &recordingWorktreeReviewer{}
			svc := prdash.NewService(prdash.Options{Reviewer: reviewer})
			if _, err := svc.UpsertWorktree(prdash.Worktree{ID: "sample", Path: "/tmp/sample", Status: tc.status, Clean: true}); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.UpsertPR(prdash.PullRequest{Number: 42, Title: "sample-pr", WorktreeID: "sample"}); err != nil {
				t.Fatal(err)
			}
			_, err := svc.RequestReview(t.Context(), 42, "")
			if tc.wantErr {
				if !errors.Is(err, prdash.ErrReviewWorktreeUnavailable) || reviewer.reviewCalls != 0 {
					t.Fatalf("error=%v calls=%d", err, reviewer.reviewCalls)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWorktreeCommitBinding(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                           string
		detached, otherHead, duplicate bool
		status                         string
		wantBinding                    bool
	}{
		{"matching branch", false, false, false, "clean", true},
		{"matching detached head", true, false, false, "clean", true},
		{"branch at other commit", false, true, false, "clean", false},
		{"detached at other commit", true, true, false, "clean", false},
		{"ambiguous detached head", true, false, true, "clean", false},
		{"unavailable detached head", true, false, false, "missing", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sha := strings.Repeat("a", 40)
			wt := prdash.Worktree{ID: "sample-wt", Path: "/tmp/sample-wt", Branch: "sample-branch", HeadSHA: sha, SessionID: "sample-session", Status: tc.status, Detached: tc.detached}
			if tc.detached {
				wt.Branch = "HEAD"
			}
			if tc.otherHead {
				wt.HeadSHA = strings.Repeat("b", 40)
			}
			worktrees := []prdash.Worktree{wt}
			if tc.duplicate {
				other := wt
				other.ID = "sample-other"
				other.Path = "/tmp/sample-other"
				worktrees = append(worktrees, other)
			}
			data := []byte(fmt.Sprintf(`[{"number":42,"title":"sample-pr","headRefName":"sample-branch","headRefOid":"%s","baseRefName":"main"}]`, sha))
			prs, err := prdash.ParseGitHubPRListJSON(data, "main", worktrees, prdash.DefaultModelPresetConfig())
			if err != nil {
				t.Fatal(err)
			}
			if len(prs) != 1 || prs[0].HeadSHA != sha {
				t.Fatalf("PR SHA was not retained: %+v", prs)
			}
			if (prs[0].WorktreeID != "") != tc.wantBinding {
				t.Fatalf("binding=%q, want bound=%v", prs[0].WorktreeID, tc.wantBinding)
			}
			if tc.wantBinding && (prs[0].WorktreeID != wt.ID || prs[0].SessionID != wt.SessionID) {
				t.Fatalf("wrong binding: %+v", prs[0])
			}
			if tc.otherHead && !tc.detached || tc.wantBinding {
				reviewer := &recordingWorktreeReviewer{}
				svc := prdash.NewService(prdash.Options{Reviewer: reviewer})
				if _, registerErr := svc.UpsertWorktree(wt); registerErr != nil {
					t.Fatal(registerErr)
				}
				pr := prs[0]
				pr.WorktreeID = "" // Exercise implicit selection again at dispatch.
				if _, registerErr := svc.UpsertPR(pr); registerErr != nil {
					t.Fatal(registerErr)
				}
				_, reviewErr := svc.RequestReview(t.Context(), 42, "")
				if tc.wantBinding {
					if reviewErr != nil || reviewer.review.WorktreePath != wt.Path {
						t.Fatalf("review error=%v target=%+v", reviewErr, reviewer.review)
					}
				} else if !errors.Is(reviewErr, prdash.ErrReviewWorktreeUnavailable) || reviewer.reviewCalls != 0 {
					t.Fatalf("mismatched worktree dispatched: %v", reviewErr)
				}
			}
		})
	}
}

func TestPRUpdatesPreserveBindings(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, session, worktree, wantSession, wantWorktree string }{
		{"omitted bindings", "", "", "sample-session", "sample-worktree"},
		{"session change", "sample-new-session", "", "sample-new-session", "sample-worktree"},
		{"explicit worktree change", "", "sample-new-worktree", "sample-session", "sample-new-worktree"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := prdash.NewService(prdash.Options{})
			if _, err := svc.UpsertPR(prdash.PullRequest{Number: 42, Title: "sample-pr", SessionID: "sample-session", WorktreeID: "sample-worktree"}); err != nil {
				t.Fatal(err)
			}
			got, err := svc.UpsertPR(prdash.PullRequest{Number: 42, Title: "sample-updated-pr", SessionID: tc.session, WorktreeID: tc.worktree})
			if err != nil {
				t.Fatal(err)
			}
			if got.SessionID != tc.wantSession || got.WorktreeID != tc.wantWorktree {
				t.Fatalf("unexpected binding: %+v", got)
			}
		})
	}
}

func TestRepositorySyncPreservesBindings(t *testing.T) {
	dir := t.TempDir()
	prepareSBNNRepository(t, dir)
	binDir := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "list" ]; then
 echo '[{"number":42,"title":"sample-pr","headRefName":"feat/sample","baseRefName":"main","state":"OPEN"}]'
else
 exit 2
fi
`
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	svc := prdash.NewService(prdash.Options{Repository: "example/sample", WorkDir: dir})
	if _, err := svc.UpsertWorktree(prdash.Worktree{ID: "sample-assigned-worktree", Path: dir, Branch: "sample-other-branch", SessionID: "sample-assigned-session"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpsertPR(prdash.PullRequest{Number: 42, Title: "sample-pr", Branch: "feat/sample", WorktreeID: "sample-assigned-worktree", SessionID: "sample-assigned-session"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := svc.SyncRepository(t.Context(), dir); err != nil {
			t.Fatal(err)
		}
		got, err := svc.GetPR(42)
		if err != nil {
			t.Fatal(err)
		}
		if got.WorktreeID != "sample-assigned-worktree" || got.SessionID != "sample-assigned-session" {
			t.Fatalf("sync replaced explicit binding: %+v", got)
		}
	}
}
