package prdash_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tenntenn/prdash/prdash"
)

func TestInitialSnapshot(t *testing.T) {
	t.Parallel()

	svc := prdash.NewService(prdash.Options{Repository: "acme/nebula-api"})
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

func TestSendPRMessage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		prNumber       int
		text           string
		expectedStatus prdash.PRStatus
		wantErr        bool
	}{
		{"auto-mode PR completes instruction and waits for human", 102, "sample-instruction-1", prdash.StatusWaitingForHuman, false},
		{"manual-mode PR enters working state", 103, "sample-instruction-2", prdash.StatusWorking, false},
		{"empty message returns error", 101, "   ", "", true},
		{"unknown PR returns error", 999, "sample-instruction-3", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := prdash.NewService(prdash.Options{Repository: "acme/nebula-api"})
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
		})
	}
}

func TestRoutineActions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		prNumber      int
		action        prdash.RoutineAction
		wantConflicts bool
		wantCIStatus  string
		wantErr       bool
	}{
		{"resolve conflicts clears conflict flag", 107, prdash.ActionResolveConflicts, false, "passing", false},
		{"fix CI transitions failing CI to passing", 109, prdash.ActionFixCI, false, "passing", false},
		{"rebase stack updates stacked chain", 102, prdash.ActionRebaseStack, false, "passing", false},
		{"invalid routine action fails", 101, prdash.RoutineAction("unknown_action"), false, "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := prdash.NewService(prdash.Options{Repository: "acme/nebula-api"})
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
			if pr.HasConflicts != tc.wantConflicts {
				t.Fatalf("expected HasConflicts=%v, got %v", tc.wantConflicts, pr.HasConflicts)
			}
			if pr.CIStatus != tc.wantCIStatus {
				t.Fatalf("expected CIStatus=%s, got %s", tc.wantCIStatus, pr.CIStatus)
			}
			if pr.Status != prdash.StatusWaitingForHuman {
				t.Fatalf("expected status %s, got %s", prdash.StatusWaitingForHuman, pr.Status)
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
			svc := prdash.NewService(prdash.Options{Repository: "acme/nebula-api"})
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
	svc := prdash.NewService(prdash.Options{Repository: "acme/nebula-api"})

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
	if pr.ReviewStatus != "approved" || pr.Status != prdash.StatusWaitingForHuman {
		t.Fatalf("expected approved & waiting_for_human, got %s / %s", pr.ReviewStatus, pr.Status)
	}

	pr, err = svc.ReportSessionResult("sess-beta", 105, prdash.StatusWaitingForHuman, "sample-session-report")
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

func TestHTTPHandler(t *testing.T) {
	t.Parallel()

	svc := prdash.NewService(prdash.Options{Repository: "acme/nebula-api"})
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

	svc := prdash.NewService(prdash.Options{Repository: "acme/nebula-api"})
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"run_routine_action","arguments":{"pr_number":107,"action":"resolve_conflicts"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"set_model_tier","arguments":{"pr_number":108,"tier":"pine"}}}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := prdash.RunMCP(t.Context(), svc, strings.NewReader(input), &out); err != nil {
		t.Fatalf("RunMCP failed: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 JSON-RPC responses, got %d: %s", len(lines), out.String())
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
	if pr107.HasConflicts {
		t.Fatal("expected PR #107 conflicts to be resolved via MCP tool call")
	}
}

func TestStateFileSynchronization(t *testing.T) {
	t.Parallel()

	statePath := t.TempDir() + "/prdash-state.json"
	webSvc := prdash.NewService(prdash.Options{
		Repository: "acme/nebula-api",
		StateFile:  statePath,
	})
	mcpSvc := prdash.NewService(prdash.Options{
		Repository: "acme/nebula-api",
		StateFile:  statePath,
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
