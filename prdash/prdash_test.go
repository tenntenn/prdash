package prdash_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tenntenn/prdash/prdash"
)

func TestWriteSyntheticStateForDocs(t *testing.T) {
	path := os.Getenv("PRDASH_WRITE_SYNTHETIC_STATE")
	if path == "" {
		t.Skip("PRDASH_WRITE_SYNTHETIC_STATE not set")
	}
	svc := prdash.NewService(prdash.Options{
		Repository: "acme/nebula-api",
		StateFile:  path,
		Reviewer:   prdash.NewStubSBNNClient(),
	})
	svc.SeedSynthetic()
}

func newSeededService(t *testing.T) *prdash.Service {
	t.Helper()
	svc := prdash.NewService(prdash.Options{
		Repository: "acme/nebula-api",
		Reviewer:   prdash.NewStubSBNNClient(),
	})
	svc.SeedSynthetic()
	return svc
}

func TestEmptyDefaultService(t *testing.T) {
	t.Parallel()

	svc := prdash.NewService(prdash.Options{Repository: "owner/repo"})
	snap := svc.Snapshot()

	if snap.Repository.FullName != "owner/repo" {
		t.Fatalf("unexpected repository full_name: %s", snap.Repository.FullName)
	}
	if len(snap.Sessions) != 0 || len(snap.Worktrees) != 0 || len(snap.PullRequests) != 0 {
		t.Fatalf("expected empty initial state without demo data, got sessions=%d worktrees=%d prs=%d",
			len(snap.Sessions), len(snap.Worktrees), len(snap.PullRequests))
	}
}

func TestInitialSnapshot(t *testing.T) {
	t.Parallel()

	svc := newSeededService(t)
	snap := svc.Snapshot()

	if snap.Repository.FullName != "acme/nebula-api" {
		t.Fatalf("unexpected repository full_name: %s", snap.Repository.FullName)
	}
	if len(snap.Sessions) != 4 {
		t.Fatalf("expected 4 worker sessions, got %d", len(snap.Sessions))
	}
	if len(snap.Worktrees) != 4 {
		t.Fatalf("expected 4 worktrees, got %d", len(snap.Worktrees))
	}
	if len(snap.PullRequests) != 9 {
		t.Fatalf("expected 9 seeded pull requests, got %d", len(snap.PullRequests))
	}
	if snap.Summary.WaitingForHuman == 0 || snap.Summary.AgentWorking == 0 {
		t.Fatalf("expected mix of waiting_for_human and working PRs, got %+v", snap.Summary)
	}

	var foundThreeTierStack bool
	for _, group := range snap.Stacks {
		if group.IsStacked && len(group.PullRequests) == 3 {
			foundThreeTierStack = true
			if group.PullRequests[0].Number != 101 || group.PullRequests[1].Number != 102 || group.PullRequests[2].Number != 103 {
				t.Fatalf("unexpected 3-tier stack order: %+v", group.PullRequests)
			}
		}
	}
	if !foundThreeTierStack {
		t.Fatal("expected a 3-tier stacked PR chain in snapshot")
	}
}

func TestDiscoveryParsersAndStackInference(t *testing.T) {
	t.Parallel()

	remoteCases := []struct {
		raw      string
		wantFull string
	}{
		{"https://github.com/example/sample-repo.git", "example/sample-repo"},
		{"git@github.com:example/sample-repo.git", "example/sample-repo"},
		{"ssh://git@github.com/example/sample-repo", "example/sample-repo"},
	}
	for _, tc := range remoteCases {
		got := prdash.ParseGitRemoteURL(tc.raw)
		if got.FullName != tc.wantFull {
			t.Fatalf("ParseGitRemoteURL(%q) = %q, want %q", tc.raw, got.FullName, tc.wantFull)
		}
	}

	porcelain := strings.Join([]string{
		"worktree /workspace/sample",
		"HEAD a1b2c3d4e5f6",
		"branch refs/heads/main",
		"",
		"worktree /workspace/sample-wt-auth",
		"HEAD 9876543210ab",
		"branch refs/heads/feat/auth-step1",
	}, "\n")
	wts := prdash.ParseGitWorktreePorcelain(porcelain)
	if len(wts) != 2 {
		t.Fatalf("expected 2 worktrees, got %d", len(wts))
	}
	if wts[1].Branch != "feat/auth-step1" || wts[1].HeadSHA != "9876543" {
		t.Fatalf("unexpected parsed worktree: %+v", wts[1])
	}
	wts[1].SessionID = "sess-auth"

	ghJSON := `[
		{"number":202,"title":"sample-stack-2","body":"step 2","headRefName":"feat/auth-step2","baseRefName":"feat/auth-step1","additions":40,"deletions":5,"changedFiles":2,"mergeable":"MERGEABLE","reviewDecision":"APPROVED","statusCheckRollup":[{"state":"SUCCESS"}]},
		{"number":201,"title":"sample-stack-1","body":"step 1","headRefName":"feat/auth-step1","baseRefName":"main","additions":80,"deletions":10,"changedFiles":3,"mergeable":"CONFLICTING","reviewDecision":"CHANGES_REQUESTED","statusCheckRollup":[{"state":"FAILURE"}]}
	]`
	prs, err := prdash.ParseGitHubPRListJSON([]byte(ghJSON), "main", wts, prdash.DefaultModelPresetConfig())
	if err != nil {
		t.Fatalf("ParseGitHubPRListJSON: %v", err)
	}
	if len(prs) != 2 {
		t.Fatalf("expected 2 PRs, got %d", len(prs))
	}
	if prs[0].Number != 201 || prs[0].StackOrder != 1 || prs[0].StackSize != 2 || !prs[0].HasConflicts || prs[0].CIStatus != "failing" || prs[0].SessionID != "sess-auth" {
		t.Fatalf("unexpected root stack PR: %+v", prs[0])
	}
	if prs[1].Number != 202 || prs[1].StackOrder != 2 || prs[1].ParentPRNumber != 201 || prs[1].ReviewStatus != "approved" {
		t.Fatalf("unexpected child stack PR: %+v", prs[1])
	}
}

func TestSendPRMessageQueuesTaskForSession(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		prNumber       int
		text           string
		expectedStatus prdash.PRStatus
		wantErr        bool
	}{
		{"sending message queues task and enters working state", 101, "sample-instruction-1", prdash.StatusWorking, false},
		{"sending message on manual-mode PR enters working state", 103, "sample-instruction-2", prdash.StatusWorking, false},
		{"empty message returns error", 101, "   ", "", true},
		{"unknown PR returns error", 999, "sample-instruction-3", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := newSeededService(t)
			updated, err := svc.SendPRMessage(tc.prNumber, "sample-author", tc.text)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if updated.Status != tc.expectedStatus {
				t.Fatalf("expected status %s, got %s", tc.expectedStatus, updated.Status)
			}
			pending := svc.PendingInstructions(updated.SessionID)
			if len(pending) == 0 || pending[len(pending)-1].Text != tc.text {
				t.Fatalf("expected instruction %q queued on session %s, got %+v", tc.text, updated.SessionID, pending)
			}
		})
	}
}

func TestRoutineActionsQueueWorkUntilSessionReportsResult(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		prNumber int
		action   prdash.RoutineAction
		wantErr  bool
	}{
		{"resolve conflicts queues routine instruction", 107, prdash.ActionResolveConflicts, false},
		{"fix CI queues routine instruction", 109, prdash.ActionFixCI, false},
		{"rebase stack queues routine instruction", 102, prdash.ActionRebaseStack, false},
		{"invalid routine action fails", 101, prdash.RoutineAction("unknown_action"), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := newSeededService(t)
			pr, err := svc.RunRoutineAction(tc.prNumber, tc.action)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if pr.Status != prdash.StatusWorking {
				t.Fatalf("expected status %s while session executes routine action, got %s", prdash.StatusWorking, pr.Status)
			}

			completed, err := svc.ReportSessionResult(pr.SessionID, tc.prNumber, prdash.StatusWaitingForHuman, "sample-routine-complete")
			if err != nil {
				t.Fatalf("ReportSessionResult: %v", err)
			}
			if completed.Status != prdash.StatusWaitingForHuman || completed.HasConflicts || completed.CIStatus != "passing" {
				t.Fatalf("unexpected PR state after session report: %+v", completed)
			}
		})
	}
}

func TestModelTierPresets(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		sessionID     string
		tier          prdash.ModelTier
		subagentTier  prdash.ModelTier
		expectedModel string
		wantErr       bool
	}{
		{"pine tier selects high-capability model", "sess-beta", prdash.TierPine, prdash.TierBamboo, "claude-opus-4-6", false},
		{"bamboo tier selects balanced model", "sess-alpha", prdash.TierBamboo, prdash.TierPlum, "claude-sonnet-4-6", false},
		{"plum tier selects fast lightweight model", "sess-gamma", prdash.TierPlum, prdash.TierPlum, "claude-haiku-4-5", false},
		{"invalid tier returns error", "sess-alpha", prdash.ModelTier("invalid"), "", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := newSeededService(t)
			sess, err := svc.SetSessionModelTier(tc.sessionID, tc.tier, tc.subagentTier)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if sess.ResolvedModel != tc.expectedModel {
				t.Fatalf("expected resolved model %s, got %s", tc.expectedModel, sess.ResolvedModel)
			}
		})
	}
}

func TestSBNNReviewAndInterSessionNotification(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	svc := newSeededService(t)

	beforeNotifs := len(svc.Snapshot().Notifications)

	pr, err := svc.PostSBNNComment(ctx, 103, "internal/auth/audit.go", 27, "sample-review-comment")
	if err != nil {
		t.Fatalf("PostSBNNComment failed: %v", err)
	}
	if pr.ReviewStatus != "changes_requested" {
		t.Fatalf("expected changes_requested, got %s", pr.ReviewStatus)
	}

	pr, err = svc.AddressReview(103, "sample-review-fix")
	if err != nil {
		t.Fatalf("AddressReview failed: %v", err)
	}
	if pr.Status != prdash.StatusWorking {
		t.Fatalf("expected working while session addresses review, got %s", pr.Status)
	}

	pr, err = svc.ReportSessionResult("sess-alpha", 103, prdash.StatusWaitingForHuman, "sample-review-fixed")
	if err != nil {
		t.Fatalf("ReportSessionResult failed: %v", err)
	}
	if pr.Status != prdash.StatusWaitingForHuman {
		t.Fatalf("expected waiting_for_human, got %s", pr.Status)
	}

	afterNotifs := len(svc.Snapshot().Notifications)
	if afterNotifs <= beforeNotifs {
		t.Fatalf("expected new human notifications to be emitted, before=%d after=%d", beforeNotifs, afterNotifs)
	}
}

func TestCLISBNNClient(t *testing.T) {
	t.Parallel()

	binDir := t.TempDir()
	fakeSBNN := filepath.Join(binDir, "fake-sbnn")
	script := "#!/bin/sh\nif [ \"$1\" = \"comments\" ]; then\n  echo '[{\"id\":\"c1\",\"path\":\"main.go\",\"startLine\":12,\"body\":\"sample-finding\",\"resolved\":false}]'\n  exit 1\nfi\nexit 0\n"
	if err := os.WriteFile(fakeSBNN, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile fake-sbnn: %v", err)
	}

	client := prdash.NewCLISBNNClient(fakeSBNN, binDir)
	cmtRes, err := client.SubmitComment(t.Context(), prdash.SBNNCommentRequest{
		Repository: "example/sample",
		PRNumber:   42,
		FilePath:   "main.go",
		Line:       12,
		Body:       "sample-comment",
	})
	if err != nil {
		t.Fatalf("SubmitComment: %v", err)
	}
	if !strings.Contains(cmtRes.Formatted, "main.go:12") {
		t.Fatalf("unexpected formatted comment: %s", cmtRes.Formatted)
	}

	revRes, err := client.RunReview(t.Context(), prdash.SBNNReviewRequest{
		Repository: "example/sample",
		PRNumber:   42,
		BaseBranch: "main",
		Branch:     "feat/sample",
	})
	if err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	if len(revRes.Findings) != 1 || !strings.Contains(revRes.Findings[0], "sample-finding") {
		t.Fatalf("unexpected review findings: %+v", revRes.Findings)
	}
}

func TestHTTPHandler(t *testing.T) {
	t.Parallel()

	svc := newSeededService(t)
	srv := httptest.NewServer(prdash.NewHandler(svc))
	defer srv.Close()

	cases := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
	}{
		{"serve index html", http.MethodGet, "/", "", http.StatusOK},
		{"get dashboard state", http.MethodGet, "/api/state", "", http.StatusOK},
		{"list pending instructions", http.MethodGet, "/api/sessions/sess-alpha/instructions", "", http.StatusOK},
		{"send PR message", http.MethodPost, "/api/prs/102/messages", `{"author":"sample-author","text":"sample-message"}`, http.StatusOK},
		{"post sbnn comment", http.MethodPost, "/api/prs/101/sbnn-comment", `{"file_path":"internal/auth/pkce.go","line":50,"body":"sample-comment"}`, http.StatusOK},
		{"send controller instruction", http.MethodPost, "/api/controller/instruction", `{"text":"sample-instruction"}`, http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), tc.method, srv.URL+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("NewRequestWithContext: %v", err)
			}
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("expected status %d, got %d", tc.wantStatus, resp.StatusCode)
			}
		})
	}
}

func TestMCPServer(t *testing.T) {
	t.Parallel()

	svc := newSeededService(t)
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"run_routine_action","arguments":{"pr_number":107,"action":"resolve_conflicts"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"report_session_result","arguments":{"session_id":"sess-gamma","pr_number":107,"status":"waiting_for_human","report":"sample-conflict-resolved"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"set_model_tier","arguments":{"pr_number":108,"tier":"pine"}}}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := prdash.RunMCP(t.Context(), svc, strings.NewReader(input), &out); err != nil {
		t.Fatalf("RunMCP failed: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 JSON-RPC responses, got %d: %s", len(lines), out.String())
	}

	for i, line := range lines {
		var payload map[string]any
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			t.Fatalf("response %d invalid JSON: %v", i, err)
		}
		if payload["error"] != nil {
			t.Fatalf("response %d returned RPC error: %v", i, payload["error"])
		}
	}

	pr107, err := svc.GetPR(107)
	if err != nil {
		t.Fatalf("GetPR(107): %v", err)
	}
	if pr107.HasConflicts || pr107.Status != prdash.StatusWaitingForHuman {
		t.Fatalf("expected PR #107 conflicts to be resolved after session report, got %+v", pr107)
	}
}

func TestStateFileSynchronization(t *testing.T) {
	t.Parallel()

	statePath := t.TempDir() + "/prdash-state.json"
	webSvc := prdash.NewService(prdash.Options{
		Repository: "acme/nebula-api",
		StateFile:  statePath,
		Reviewer:   prdash.NewStubSBNNClient(),
	})
	webSvc.SeedSynthetic()
	mcpSvc := prdash.NewService(prdash.Options{
		Repository: "acme/nebula-api",
		StateFile:  statePath,
		Reviewer:   prdash.NewStubSBNNClient(),
	})

	if _, err := mcpSvc.ReportSessionResult("sess-delta", 109, prdash.StatusWaitingForHuman, "sample-sync-report"); err != nil {
		t.Fatalf("ReportSessionResult: %v", err)
	}

	pr109, err := webSvc.GetPR(109)
	if err != nil {
		t.Fatalf("webSvc.GetPR(109): %v", err)
	}
	if pr109.Status != prdash.StatusWaitingForHuman {
		t.Fatalf("expected webSvc to observe status %s, got %s", prdash.StatusWaitingForHuman, pr109.Status)
	}
}

func TestUpsertWorktreeSessionAndPR(t *testing.T) {
	t.Parallel()

	svc := prdash.NewService(prdash.Options{Repository: "acme/nebula-api"})

	wt, err := svc.UpsertWorktree(prdash.Worktree{
		ID:        "wt-sample",
		Path:      "/workspace/repos/sample",
		Branch:    "feat/sample-branch",
		HeadSHA:   "a719dfd",
		Clean:     true,
		SessionID: "sess-sample",
	})
	if err != nil {
		t.Fatalf("UpsertWorktree: %v", err)
	}
	if wt.ID != "wt-sample" {
		t.Fatalf("unexpected worktree id: %s", wt.ID)
	}

	sess, err := svc.UpsertSession(prdash.Session{
		ID:                "sess-sample",
		Name:              "sample-session",
		WorktreeID:        "wt-sample",
		ModelTier:         prdash.TierPine,
		SubagentModelTier: prdash.TierBamboo,
		ActiveTask:        "sample-task",
	})
	if err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	if sess.ResolvedModel != "claude-opus-4-6" {
		t.Fatalf("unexpected resolved model: %s", sess.ResolvedModel)
	}

	pr, err := svc.UpsertPR(prdash.PullRequest{
		Number:      10,
		Title:       "sample-pr-title",
		Description: "sample-pr-description",
		Branch:      "feat/sample-branch",
		BaseBranch:  "main",
		SessionID:   "sess-sample",
		WorktreeID:  "wt-sample",
		Status:      prdash.StatusWaitingForHuman,
		AutoMode:    true,
		ModelTier:   prdash.TierPine,
	})
	if err != nil {
		t.Fatalf("UpsertPR: %v", err)
	}
	if pr.Number != 10 || pr.Status != prdash.StatusWaitingForHuman {
		t.Fatalf("unexpected PR state: %+v", pr)
	}
}
