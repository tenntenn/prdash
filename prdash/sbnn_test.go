package prdash_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tenntenn/prdash/prdash"
)

// The test binary acts as a CLI process so protocol failures are exercised at
// the real exec boundary without depending on an installed sbnn or curl.
func TestSBNNCommandProcess(t *testing.T) {
	base := os.Getenv("PRDASH_TEST_SBNN")
	if base == "" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 2 {
		os.Exit(91)
	}
	args = args[1:]
	path := ""
	payload := map[string]any{}
	switch args[0] {
	case "--status":
		path = "/status"
	case "--target":
		path = "/send"
		labels := map[string]string{}
		for i := 0; i < len(args); i++ {
			switch args[i] {
			case "--target":
				i++
				payload["target"] = args[i]
			case "--label":
				i++
				key, value, _ := strings.Cut(args[i], "=")
				labels[key] = value
			}
		}
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(92)
		}
		payload["raw"] = string(raw)
		payload["labels"] = labels
	case "comment":
		path = "/comment"
	default:
		os.Exit(93)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		os.Exit(94)
	}
	resp, err := http.Post(base+path, "application/json", bytes.NewReader(data))
	if err != nil {
		os.Exit(95)
	}
	_, copyErr := io.Copy(os.Stdout, resp.Body)
	closeErr := resp.Body.Close()
	if copyErr != nil || closeErr != nil {
		os.Exit(96)
	}
	if resp.StatusCode != http.StatusOK {
		os.Exit(2)
	}
	os.Exit(0)
}

type fixtureDiff struct {
	ID        string            `json:"id"`
	Raw       string            `json:"raw"`
	Labels    map[string]string `json:"labels"`
	CreatedAt time.Time         `json:"createdAt"`
}

type fixtureGroup struct {
	Name          string           `json:"name"`
	Diffs         []fixtureDiff    `json:"diffs"`
	Comments      []map[string]any `json:"comments"`
	ReviewedAt    time.Time        `json:"reviewedAt"`
	ReviewVerdict string           `json:"reviewVerdict"`
}

type sbnnFixture struct {
	mu                              sync.Mutex
	groups                          map[string]*fixtureGroup
	url, bin                        string
	sends, comments                 int
	statusCode, sendCode, groupCode int
	statusRaw, sendRaw, groupRaw    string
}

func newSBNNFixture(t *testing.T) *sbnnFixture {
	t.Helper()
	f := &sbnnFixture{groups: make(map[string]*fixtureGroup)}
	server := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(server.Close)
	f.mu.Lock()
	f.url = server.URL
	f.mu.Unlock()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f.bin = filepath.Join(t.TempDir(), "sample-sbnn")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := "#!/bin/sh\nPRDASH_TEST_SBNN=" + quote(f.url) + " GORACE=atexit_sleep_ms=0 exec " + quote(binary) + " -test.run '^TestSBNNCommandProcess$' -- \"$@\"\n"
	if err := os.WriteFile(f.bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *sbnnFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/status":
		if f.statusCode != 0 {
			w.WriteHeader(f.statusCode)
			return
		}
		if f.statusRaw != "" {
			fmt.Fprint(w, f.statusRaw)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"app": "sbnn", "url": f.url})
	case "/send":
		f.sends++
		if f.sendCode != 0 {
			w.WriteHeader(f.sendCode)
			return
		}
		if f.sendRaw != "" {
			fmt.Fprint(w, f.sendRaw)
			return
		}
		var input struct {
			Target string            `json:"target"`
			Raw    string            `json:"raw"`
			Labels map[string]string `json:"labels"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		group := f.groups[input.Target]
		if group == nil {
			group = &fixtureGroup{Name: input.Target, Comments: []map[string]any{}}
			f.groups[input.Target] = group
		}
		diff := fixtureDiff{ID: fmt.Sprintf("sample-diff-%d", f.sends), Raw: input.Raw, Labels: input.Labels, CreatedAt: time.Now().UTC()}
		group.Diffs = append(group.Diffs, diff)
		json.NewEncoder(w).Encode(map[string]any{"group": input.Target, "url": f.url + "/" + input.Target, "diff": map[string]string{"id": diff.ID}})
	case "/comment":
		f.comments++
		fmt.Fprint(w, "{}")
	default:
		if !strings.HasPrefix(r.URL.Path, "/_/api/groups/") {
			http.NotFound(w, r)
			return
		}
		if f.groupCode != 0 {
			w.WriteHeader(f.groupCode)
			return
		}
		if f.groupRaw != "" {
			fmt.Fprint(w, f.groupRaw)
			return
		}
		group := f.groups[strings.TrimPrefix(r.URL.Path, "/_/api/groups/")]
		if group == nil {
			http.NotFound(w, r)
			return
		}
		// The live API omits raw text, while the persisted record retains it.
		response := *group
		response.Diffs = append([]fixtureDiff(nil), group.Diffs...)
		for i := range response.Diffs {
			response.Diffs[i].Raw = ""
		}
		json.NewEncoder(w).Encode(response)
	}
}

func prepareReviewRepository(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	reviewGit(t, dir, "init", "--initial-branch=main")
	reviewGit(t, dir, "-c", "user.name=sample", "-c", "user.email=sample@example.com", "commit", "--allow-empty", "-m", "sample base")
	reviewGit(t, dir, "switch", "-c", "sample-branch")
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte("package sample\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reviewGit(t, dir, "add", "--", filepath.Join(dir, "sample.go"))
	reviewGit(t, dir, "-c", "user.name=sample", "-c", "user.email=sample@example.com", "commit", "-m", "sample change")
	return dir, reviewGit(t, dir, "rev-parse", "HEAD")
}

func reviewGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestSBNNSubmissionAndRestart(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, verdict string
		submitted     bool
		findings      bool
		want          string
	}{
		{"pending", "", false, false, "pending"},
		{"draft comments", "", false, true, "pending"},
		{"approved", "approved", true, false, "approved"},
		{"commented", "commented", true, false, "commented"},
		{"changes without comments", "changes-requested", true, false, "changes_requested"},
		{"changes with comments", "changes-requested", true, true, "changes_requested"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, sha := prepareReviewRepository(t)
			f := newSBNNFixture(t)
			req := prdash.SBNNReviewRequest{Repository: "example/sample", PRNumber: 42, Branch: "sample-branch", BaseBranch: "main", HeadSHA: sha}
			first, err := prdash.NewCLISBNNClient(f.bin, dir).RunReview(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			if first.Reviewed || first.Verdict != "pending" {
				t.Fatalf("delivery was treated as submission: %+v", first)
			}
			f.mu.Lock()
			for _, group := range f.groups {
				if tc.submitted {
					group.ReviewedAt = time.Now().UTC()
					group.ReviewVerdict = tc.verdict
				}
				if tc.findings {
					group.Comments = append(group.Comments, map[string]any{"id": "sample-comment", "path": "sample.go", "startLine": 1, "body": "sample-finding", "resolved": false})
				}
			}
			f.mu.Unlock()
			// A new client has no prior delivery cache, as after a restart.
			got, err := prdash.NewCLISBNNClient(f.bin, dir).RunReview(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			if got.Reviewed != tc.submitted || got.Verdict != tc.want || got.ReviewedCommit != sha || got.GroupURL != first.GroupURL {
				t.Fatalf("unexpected result: %+v", got)
			}
			if (len(got.Findings) > 0) != tc.findings {
				t.Fatalf("findings lost: %+v", got)
			}
			f.mu.Lock()
			sends := f.sends
			f.mu.Unlock()
			if sends != 1 {
				t.Fatalf("duplicate diff sent after restart: %d", sends)
			}
		})
	}
}

func TestSBNNProtocolFailures(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, kind string }{
		{"status exit", "status_exit"}, {"status JSON", "status_json"}, {"delivery exit", "send_exit"},
		{"delivery JSON", "send_json"}, {"group JSON", "group_json"}, {"group HTTP failure", "group_http"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, sha := prepareReviewRepository(t)
			f := newSBNNFixture(t)
			switch tc.kind {
			case "status_exit":
				f.statusCode = 500
			case "status_json":
				f.statusRaw = "{"
			case "send_exit":
				f.sendCode = 500
			case "send_json":
				f.sendRaw = "{"
			case "group_json":
				f.groupRaw = "{"
			case "group_http":
				f.groupCode = 500
			}
			_, err := prdash.NewCLISBNNClient(f.bin, dir).RunReview(t.Context(), prdash.SBNNReviewRequest{Repository: "example/sample", PRNumber: 42, BaseBranch: "main", HeadSHA: sha})
			if err == nil {
				t.Fatal("expected protocol failure")
			}
		})
	}
}

func TestSBNNRepositoryIsolationAndConcurrentRequests(t *testing.T) {
	t.Parallel()
	dir, sha := prepareReviewRepository(t)
	f := newSBNNFixture(t)
	t.Run("parallel clients", func(t *testing.T) {
		for i := range 8 {
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				t.Parallel()
				_, err := prdash.NewCLISBNNClient(f.bin, dir).RunReview(t.Context(), prdash.SBNNReviewRequest{Repository: "example/sample", PRNumber: 42, BaseBranch: "main", HeadSHA: sha})
				if err != nil {
					t.Error(err)
				}
			})
		}
	})
	_, err := prdash.NewCLISBNNClient(f.bin, dir).RunReview(t.Context(), prdash.SBNNReviewRequest{Repository: "example/other", PRNumber: 42, BaseBranch: "main", HeadSHA: sha})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sends != 2 || len(f.groups) != 2 {
		t.Fatalf("sends=%d groups=%d", f.sends, len(f.groups))
	}
}

func TestSBNNPinnedCommitAndNextRound(t *testing.T) {
	t.Parallel()
	dir, sha := prepareReviewRepository(t)
	f := newSBNNFixture(t)
	client := prdash.NewCLISBNNClient(f.bin, dir)
	req := prdash.SBNNReviewRequest{Repository: "example/sample", PRNumber: 42, Branch: "sample-branch", BaseBranch: "main", HeadSHA: sha}
	if _, err := client.RunReview(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	for _, group := range f.groups {
		group.ReviewedAt = time.Now().UTC()
		group.ReviewVerdict = "approved"
		group.Comments = append(group.Comments, map[string]any{"body": "sample-outstanding"})
	}
	f.mu.Unlock()
	if err := os.WriteFile(filepath.Join(dir, "later.go"), []byte("package sample\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reviewGit(t, dir, "add", "--", filepath.Join(dir, "later.go"))
	reviewGit(t, dir, "-c", "user.name=sample", "-c", "user.email=sample@example.com", "commit", "-m", "sample next change")
	if _, err := client.RunReview(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	for _, group := range f.groups {
		if strings.Contains(group.Diffs[0].Raw, "later.go") {
			t.Error("pinned diff includes newer commit")
		}
	}
	f.mu.Unlock()
	req.HeadSHA = reviewGit(t, dir, "rev-parse", "HEAD")
	got, err := client.RunReview(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reviewed || got.Verdict != "pending" || len(got.Findings) != 1 {
		t.Fatalf("new round lost comments or inherited approval: %+v", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sends != 2 {
		t.Fatalf("sends=%d", f.sends)
	}
}

func TestSBNNServiceCompletionAndPersistence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, verdict, wantStatus string
		reviewed                  bool
	}{
		{"pending", "pending", "review_pending", false},
		{"approved", "approved", "approved", true},
		{"commented", "commented", "reviewed", true},
		{"changes without inline findings", "changes_requested", "changes_requested", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reviewer := &resultReviewer{result: prdash.SBNNReviewResult{Reviewed: tc.reviewed, Verdict: tc.verdict, GroupURL: "http://localhost:6280/sample", ReviewedCommit: "sample-head", CreatedAt: time.Now().UTC()}}
			opts := prdash.Options{Repository: "example/sample", Reviewer: reviewer, StateFile: filepath.Join(t.TempDir(), "state.json")}
			svc := prdash.NewService(opts)
			if _, err := svc.UpsertPR(prdash.PullRequest{Number: 42, Title: "sample-pr", HeadSHA: "sample-head", AutoMode: true, CIStatus: "failing", HasConflicts: true}); err != nil {
				t.Fatal(err)
			}
			got, err := svc.RequestReview(t.Context(), 42, "")
			if err != nil {
				t.Fatal(err)
			}
			if got.ReviewStatus != tc.wantStatus || got.CIStatus != "failing" || !got.HasConflicts || len(got.SBNNReviews) != 1 || got.SBNNGroupURL != reviewer.result.GroupURL {
				t.Fatalf("unexpected result: %+v", got)
			}
			if len(svc.PendingInstructions("")) != 0 {
				t.Fatal("unexpected automatic instruction without actionable findings")
			}
			restored, err := prdash.NewService(opts).GetPR(42)
			if err != nil {
				t.Fatal(err)
			}
			if len(restored.SBNNReviews) != 1 || restored.SBNNReviews[0].Reviewed != tc.reviewed || restored.SBNNReviews[0].ReviewedCommit != "sample-head" || restored.SBNNReviews[0].Verdict != tc.verdict {
				t.Fatalf("review metadata not persisted: %+v", restored)
			}
		})
	}
}

type resultReviewer struct {
	prdash.StubSBNNClient
	result     prdash.SBNNReviewResult
	service    *prdash.Service
	changeHead bool
	request    prdash.SBNNReviewRequest
}

func (r *resultReviewer) RunReview(_ context.Context, req prdash.SBNNReviewRequest) (prdash.SBNNReviewResult, error) {
	r.request = req
	if r.changeHead {
		pr, err := r.service.GetPR(req.PRNumber)
		if err != nil {
			return prdash.SBNNReviewResult{}, err
		}
		pr.HeadSHA = "sample-new-head"
		if _, err := r.service.UpsertPR(pr); err != nil {
			return prdash.SBNNReviewResult{}, err
		}
	}
	return r.result, nil
}

func TestSBNNServiceRejectsStaleResult(t *testing.T) {
	t.Parallel()
	reviewer := &resultReviewer{changeHead: true, result: prdash.SBNNReviewResult{Reviewed: true, Verdict: "approved", ReviewedCommit: "sample-head"}}
	svc := prdash.NewService(prdash.Options{Reviewer: reviewer})
	reviewer.service = svc
	if _, err := svc.UpsertPR(prdash.PullRequest{Number: 42, Title: "sample-pr", HeadSHA: "sample-head", ReviewStatus: "changes_requested", Status: prdash.StatusWorking}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RequestReview(t.Context(), 42, ""); err == nil {
		t.Fatal("expected stale review error")
	}
	got, err := svc.GetPR(42)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReviewStatus != "changes_requested" || got.Status != prdash.StatusWorking || got.HeadSHA != "sample-new-head" || len(got.SBNNReviews) != 1 || !got.SBNNReviews[0].Stale {
		t.Fatalf("stale result affected current state: %+v", got)
	}
}

func TestSBNNServiceUsesAssignedWorktree(t *testing.T) {
	t.Parallel()
	reviewer := &resultReviewer{result: prdash.SBNNReviewResult{ReviewedCommit: "sample-head", Verdict: "pending"}}
	svc := prdash.NewService(prdash.Options{Reviewer: reviewer})
	assigned := t.TempDir()
	for _, wt := range []prdash.Worktree{
		{ID: "sample-other", Path: t.TempDir(), Branch: "sample-branch"},
		{ID: "sample-assigned", Path: assigned, Branch: "sample-branch"},
	} {
		if _, err := svc.UpsertWorktree(wt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.UpsertPR(prdash.PullRequest{Number: 42, Title: "sample-pr", HeadSHA: "sample-head", Branch: "sample-branch", BaseBranch: "sample-base", WorktreeID: "sample-assigned"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RequestReview(t.Context(), 42, ""); err != nil {
		t.Fatal(err)
	}
	if reviewer.request.WorktreePath != assigned || reviewer.request.HeadSHA != "sample-head" || reviewer.request.BaseBranch != "sample-base" {
		t.Fatalf("wrong execution target: %+v", reviewer.request)
	}
}

func TestSBNNCommentReusesDeliveredDiff(t *testing.T) {
	t.Parallel()
	dir, sha := prepareReviewRepository(t)
	f := newSBNNFixture(t)
	client := prdash.NewCLISBNNClient(f.bin, dir)
	review, err := client.RunReview(t.Context(), prdash.SBNNReviewRequest{Repository: "example/sample", PRNumber: 42, BaseBranch: "main", HeadSHA: sha})
	if err != nil {
		t.Fatal(err)
	}
	comment, err := client.SubmitComment(t.Context(), prdash.SBNNCommentRequest{Repository: "example/sample", PRNumber: 42, BaseBranch: "main", HeadSHA: sha, FilePath: "sample.go", Line: 1, Body: "sample-comment"})
	if err != nil {
		t.Fatal(err)
	}
	if comment.GroupURL != review.GroupURL || comment.ReviewedCommit != sha {
		t.Fatalf("comment target mismatch: %+v", comment)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sends != 1 || f.comments != 1 {
		t.Fatalf("sends=%d comments=%d", f.sends, f.comments)
	}
}

func TestSBNNAcquisitionFailureDoesNotDeliver(t *testing.T) {
	t.Parallel()
	cases := []string{"missing commit", "missing base", "empty diff"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, sha := prepareReviewRepository(t)
			f := newSBNNFixture(t)
			req := prdash.SBNNReviewRequest{Repository: "example/sample", PRNumber: 42, HeadSHA: sha, BaseBranch: "main"}
			switch name {
			case "missing commit":
				req.HeadSHA = "sample-missing-commit"
			case "missing base":
				req.BaseBranch = "sample-missing-base"
			case "empty diff":
				req.BaseBranch = sha
			}
			if _, err := prdash.NewCLISBNNClient(f.bin, dir).RunReview(t.Context(), req); err == nil {
				t.Fatal("expected acquisition error")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.sends != 0 {
				t.Fatalf("delivered after acquisition failed: %d", f.sends)
			}
		})
	}
}

func TestSBNNApprovedCommentsRemainNonblocking(t *testing.T) {
	t.Parallel()
	reviewer := &resultReviewer{result: prdash.SBNNReviewResult{Reviewed: true, Verdict: "approved", Findings: []string{"sample-nonblocking-comment"}}}
	svc := prdash.NewService(prdash.Options{Reviewer: reviewer})
	if _, err := svc.UpsertPR(prdash.PullRequest{Number: 42, Title: "sample-pr", AutoMode: true}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.RequestReview(t.Context(), 42, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.ReviewStatus != "approved" || len(got.SBNNReviews[0].Findings) != 1 || len(svc.PendingInstructions("")) != 0 {
		t.Fatalf("approval with comments was changed: %+v", got)
	}
	got.SBNNReviews[0].Findings[0] = "sample-mutation"
	stored, err := svc.GetPR(42)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SBNNReviews[0].Findings[0] != "sample-nonblocking-comment" {
		t.Fatal("review result aliases stored findings")
	}
}

func TestSBNNGitHubFallbackCommitChecks(t *testing.T) {
	cases := []struct {
		name, afterHead, afterBase string
		wantErr                    bool
	}{
		{"stable", strings.Repeat("a", 40), strings.Repeat("c", 40), false},
		{"head changed", strings.Repeat("b", 40), strings.Repeat("c", 40), true},
		{"base changed", strings.Repeat("a", 40), strings.Repeat("d", 40), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			f := newSBNNFixture(t)
			script := fmt.Sprintf(`#!/bin/sh
dir=$(dirname "$0")
if [ "$2" = "view" ]; then
 if [ -e "$dir/viewed" ]; then
  echo '{"headRefOid":"%s","baseRefOid":"%s"}'
 else
  touch "$dir/viewed"
  echo '{"headRefOid":"%s","baseRefOid":"%s"}'
 fi
elif [ "$2" = "diff" ]; then
 echo 'diff --git a/sample.go b/sample.go
--- a/sample.go
+++ b/sample.go
@@ -0,0 +1 @@
+package sample'
else
 exit 2
fi
`, tc.afterHead, tc.afterBase, strings.Repeat("a", 40), strings.Repeat("c", 40))
			if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			got, err := prdash.NewCLISBNNClient(f.bin, dir).RunReview(t.Context(), prdash.SBNNReviewRequest{Repository: "example/sample", PRNumber: 42, Branch: "sample-branch", BaseBranch: "main"})
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected changed target error")
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				if f.sends != 0 {
					t.Fatal("sent a moving GitHub diff")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if got.ReviewedCommit != tc.afterHead {
					t.Fatalf("wrong commit: %+v", got)
				}
			}
		})
	}
}
