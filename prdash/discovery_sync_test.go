package prdash

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitHubUnknownAndLifecycle(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, input, state, ci, mergeability string
		draft                                bool
	}{
		{"unknown", `[{"number":1}]`, "unknown", "unknown", "unknown", false},
		{"draft", `[{"number":1,"state":"OPEN","isDraft":true,"mergeable":"UNKNOWN","statusCheckRollup":[{}]}]`, "open", "unknown", "unknown", true},
		{"merged", `[{"number":1,"state":"MERGED","mergeable":"MERGEABLE","statusCheckRollup":[{"conclusion":"SUCCESS"}]}]`, "merged", "passing", "mergeable", false},
		{"closed", `[{"number":1,"state":"CLOSED","mergeable":"CONFLICTING","statusCheckRollup":[{"conclusion":"FAILURE"}]}]`, "closed", "failing", "conflicting", false},
		{"running", `[{"number":1,"state":"OPEN","statusCheckRollup":[{"status":"IN_PROGRESS"}]}]`, "open", "running", "unknown", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			prs, err := ParseGitHubPRListJSON([]byte(tt.input), "main", nil, DefaultModelPresetConfig())
			if err != nil {
				t.Fatal(err)
			}
			pr := prs[0]
			if pr.GitHubState != tt.state || pr.CIStatus != tt.ci || pr.Mergeability != tt.mergeability || pr.IsDraft != tt.draft {
				t.Fatalf("unexpected PR: %+v", pr)
			}
			if pr.HasConflicts != (tt.mergeability == "conflicting") {
				t.Fatalf("conflict flag: %v", pr.HasConflicts)
			}
		})
	}
}

// Each test owns a fake gh executable; only PATH is changed, never the cwd.
func fakeSyncGH(t *testing.T, list []byte, failList bool) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "list.json"), list, 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
root="$(dirname "$0")"
printf '%s\n' "$*" >> "$root/calls"
if [ "$2" = "list" ]; then
 if [ -f "$root/fail-list" ]; then exit 1; fi
 cat "$root/list.json"
elif [ "$2" = "view" ]; then
 if [ -f "$root/view-$3.json" ]; then cat "$root/view-$3.json"; else exit 1; fi
else
 exit 2
fi
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if failList {
		if err := os.WriteFile(filepath.Join(dir, "fail-list"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestDiscoveryTruncationAndTrackedLifecycle(t *testing.T) {
	items := make([]ghPRItem, githubPRLimit+1)
	for i := range items {
		items[i] = ghPRItem{Number: i + 1, Title: "sample", State: "OPEN"}
	}
	data, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	bin := fakeSyncGH(t, data, false)
	if err := os.WriteFile(filepath.Join(bin, "view-500.json"), []byte(`{"number":500,"title":"merged sample","state":"MERGED"}`), 0600); err != nil {
		t.Fatal(err)
	}
	prs, err := discoverTrackedPullRequests(t.Context(), "example/sample", "", "main", nil, DefaultModelPresetConfig(), []int{1, 500, 600})
	if !errors.Is(err, ErrPRResultsTruncated) || !strings.Contains(err.Error(), "view 600") {
		t.Fatalf("expected truncation and partial failure, got %v", err)
	}
	if len(prs) != githubPRLimit+1 || prs[len(prs)-1].GitHubState != "merged" {
		t.Fatalf("partial results lost: %d", len(prs))
	}
	calls, err := os.ReadFile(filepath.Join(bin, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(calls, []byte("--limit 101")) || bytes.Contains(calls, []byte("view 1 ")) {
		t.Fatalf("unexpected requests: %s", calls)
	}
}

func TestSyncPartialGitFailureAndCooldown(t *testing.T) {
	bin := fakeSyncGH(t, []byte(`[{"number":1,"title":"sample","state":"OPEN","isDraft":true}]`), false)
	svc := NewService(Options{Repository: "example/sample", WorkDir: t.TempDir()})
	snap, err := svc.SyncRepository(t.Context(), "")
	if err == nil || snap.Sync.GitError == "" || snap.Sync.GitHubError != "" || len(snap.PullRequests) != 1 || !snap.PullRequests[0].IsDraft {
		t.Fatalf("partial sync: %+v, %v", snap.Sync, err)
	}
	calls, readErr := os.ReadFile(filepath.Join(bin, "calls"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	cached, cachedErr := svc.SyncRepository(t.Context(), "")
	if cachedErr == nil || cached.Sync.AttemptedAt != snap.Sync.AttemptedAt {
		t.Fatalf("cooldown did not retain failure: %v", cachedErr)
	}
	after, readErr := os.ReadFile(filepath.Join(bin, "calls"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(calls, after) {
		t.Fatal("cooldown made additional GitHub calls")
	}
	svc.syncMu.Lock()
	_, busyErr := svc.SyncRepository(t.Context(), "")
	svc.syncMu.Unlock()
	if busyErr == nil || !strings.Contains(busyErr.Error(), "in progress") {
		t.Fatalf("expected busy error, got %v", busyErr)
	}
}

func TestSyncGitHubPartialFailurePreservesTrackedPR(t *testing.T) {
	bin := fakeSyncGH(t, nil, true)
	if err := os.WriteFile(filepath.Join(bin, "view-2.json"), []byte(`{"number":2,"title":"closed sample","state":"CLOSED"}`), 0600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "git", "-C", dir, "init", "-b", "main")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	svc := NewService(Options{Repository: "example/sample", WorkDir: dir})
	for _, n := range []int{2, 3} {
		if _, err := svc.UpsertPR(PullRequest{Number: n, Title: "original sample", GitHubState: "open", CIStatus: "failing"}); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := svc.SyncRepository(t.Context(), "")
	if err == nil || snap.Sync.GitError != "" || snap.Sync.GitHubError == "" || len(snap.Worktrees) != 1 {
		t.Fatalf("partial sync: %+v %v", snap.Sync, err)
	}
	for _, pr := range snap.PullRequests {
		if pr.Number == 2 && pr.GitHubState != "closed" {
			t.Fatalf("closed PR not refreshed: %+v", pr)
		}
		if pr.Number == 3 && (pr.GitHubState != "open" || pr.CIStatus != "failing") {
			t.Fatalf("unavailable PR overwritten: %+v", pr)
		}
	}
	if snap.Sync.NextAllowedAt.Sub(snap.Sync.CompletedAt) < 29*time.Second {
		t.Fatalf("missing cooldown: %+v", snap.Sync)
	}
}

func TestSyncHTTPPartialFailureAndCachedError(t *testing.T) {
	bin := fakeSyncGH(t, []byte(`[{"number":1,"title":"sample","state":"OPEN"}]`), false)
	svc := NewService(Options{Repository: "example/sample", WorkDir: t.TempDir()})
	handler := NewHandler(svc)
	var previous []byte
	for range 2 {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/sync", strings.NewReader(`{}`)))
		if response.Code < 400 {
			t.Fatalf("partial failure hidden: %d %s", response.Code, response.Body.String())
		}
		calls, err := os.ReadFile(filepath.Join(bin, "calls"))
		if err != nil {
			t.Fatal(err)
		}
		if previous != nil && !bytes.Equal(previous, calls) {
			t.Fatal("repeated HTTP request repeated GitHub query")
		}
		previous = calls
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	var snapshot Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Sync.GitError == "" || len(snapshot.PullRequests) != 1 {
		t.Fatalf("available data and diagnostic lost: %+v", snapshot)
	}
}

func TestDiscoveryExactlyAtLimitIsComplete(t *testing.T) {
	items := make([]ghPRItem, githubPRLimit)
	for i := range items {
		items[i] = ghPRItem{Number: i + 1, State: "OPEN"}
	}
	data, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	fakeSyncGH(t, data, false)
	prs, err := DiscoverPullRequests(t.Context(), "example/sample", "", "main", nil, DefaultModelPresetConfig())
	if err != nil || len(prs) != githubPRLimit {
		t.Fatalf("exact limit incorrectly truncated: %d %v", len(prs), err)
	}
}
