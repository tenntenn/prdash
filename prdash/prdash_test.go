package prdash_test

import (
	"bytes"
	"context"
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
		{
			name:           "auto-mode PR completes instruction and waits for human",
			prNumber:       102,
			text:           "Add test for invalid refresh token signature",
			expectedStatus: prdash.StatusWaitingForHuman,
			wantErr:        false,
		},
		{
			name:           "manual-mode PR enters working state",
			prNumber:       103,
			text:           "Include actor IP in revocation audit event",
			expectedStatus: prdash.StatusWorking,
			wantErr:        false,
		},
		{
			name:     "empty message returns error",
			prNumber: 101,
			text:     "   ",
			wantErr:  true,
		},
		{
			name:     "unknown PR returns error",
			prNumber: 999,
			text:     "Check status",
			wantErr:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := prdash.NewService(prdash.Options{Repository: "acme/nebula-api"})
			updated, err := svc.SendPRMessage(tc.prNumber, "Reviewer", tc.text)
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
		{
			name:          "resolve conflicts clears conflict flag",
			prNumber:      107,
			action:        prdash.ActionResolveConflicts,
			wantConflicts: false,
			wantCIStatus:  "passing",
			wantErr:       false,
		},
		{
			name:          "fix CI transitions failing CI to passing",
			prNumber:      109,
			action:        prdash.ActionFixCI,
			wantConflicts: false,
			wantCIStatus:  "passing",
			wantErr:       false,
		},
		{
			name:          "rebase stack updates stacked chain",
			prNumber:      102,
			action:        prdash.ActionRebaseStack,
			wantConflicts: false,
			wantCIStatus:  "passing",
			wantErr:       false,
		},
		{
			name:     "invalid routine action fails",
			prNumber: 101,
			action:   prdash.RoutineAction("unknown_action"),
			wantErr:  true,
		},
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
		{
			name:          "pine tier selects high-capability model",
			sessionID:     "sess-beta",
			tier:          prdash.TierPine,
			subagentTier:  prdash.TierBamboo,
			expectedModel: "claude-opus-4-6",
			wantErr:       false,
		},
		{
			name:          "bamboo tier selects balanced model",
			sessionID:     "sess-alpha",
			tier:          prdash.TierBamboo,
			subagentTier:  prdash.TierPlum,
			expectedModel: "claude-sonnet-4-6",
			wantErr:       false,
		},
		{
			name:          "plum tier selects fast lightweight model",
			sessionID:     "sess-gamma",
			tier:          prdash.TierPlum,
			subagentTier:  prdash.TierPlum,
			expectedModel: "claude-haiku-4-5",
			wantErr:       false,
		},
		{
			name:      "invalid tier returns error",
			sessionID: "sess-alpha",
			tier:      prdash.ModelTier("invalid"),
			wantErr:   true,
		},
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

	ctx := context.Background()
	svc := prdash.NewService(prdash.Options{Repository: "acme/nebula-api"})

	beforeNotifs := len(svc.Snapshot().Notifications)

	// Post an sbnn comment on manual-mode PR #103, then address review
	pr, err := svc.PostSBNNComment(ctx, 103, "internal/auth/audit.go", 27, "Include request correlation ID in audit payload")
	if err != nil {
		t.Fatalf("PostSBNNComment failed: %v", err)
	}
	if pr.ReviewStatus != "changes_requested" {
		t.Fatalf("expected changes_requested, got %s", pr.ReviewStatus)
	}

	pr, err = svc.AddressReview(103, "Add correlation_id field to AuditEvent")
	if err != nil {
		t.Fatalf("AddressReview failed: %v", err)
	}
	if pr.ReviewStatus != "approved" || pr.Status != prdash.StatusWaitingForHuman {
		t.Fatalf("expected approved & waiting_for_human, got %s / %s", pr.ReviewStatus, pr.Status)
	}

	// Report session completion on PR #105 (working -> waiting_for_human)
	pr, err = svc.ReportSessionResult("sess-beta", 105, prdash.StatusWaitingForHuman, "Completed retention benchmarks")
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
		{
			name:       "serve index html",
			method:     http.MethodGet,
			path:       "/",
			wantStatus: http.StatusOK,
		},
		{
			name:       "get dashboard state",
			method:     http.MethodGet,
			path:       "/api/state",
			wantStatus: http.StatusOK,
		},
		{
			name:       "send PR message",
			method:     http.MethodPost,
			path:       "/api/prs/102/messages",
			body:       `{"author":"Human","text":"Verify token expiry edge case"}`,
			wantStatus: http.StatusOK,
		},
		{
			name:       "post sbnn comment",
			method:     http.MethodPost,
			path:       "/api/prs/101/sbnn-comment",
			body:       `{"file_path":"internal/auth/pkce.go","line":50,"body":"Check verifier length bounds"}`,
			wantStatus: http.StatusOK,
		},
		{
			name:       "send controller instruction",
			method:     http.MethodPost,
			path:       "/api/controller/instruction",
			body:       `{"text":"Prioritize the OAuth2 stack first"}`,
			wantStatus: http.StatusOK,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, srv.URL+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
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
	if err := prdash.RunMCP(context.Background(), svc, strings.NewReader(input), &out); err != nil {
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

	if _, err := mcpSvc.ReportSessionResult("sess-delta", 109, prdash.StatusWaitingForHuman, "Fixed idempotency test race via MCP"); err != nil {
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

	svc := prdash.NewService(prdash.Options{Repository: "tenntenn/prdash"})

	wt, err := svc.UpsertWorktree(prdash.Worktree{
		ID:        "wt-dogfood",
		Path:      "/workspace/repos/prdash",
		Branch:    "feat/dynamic-registration",
		HeadSHA:   "a719dfd",
		Clean:     true,
		SessionID: "sess-dogfood",
	})
	if err != nil {
		t.Fatalf("UpsertWorktree: %v", err)
	}
	if wt.ID != "wt-dogfood" {
		t.Fatalf("unexpected worktree id: %s", wt.ID)
	}

	sess, err := svc.UpsertSession(prdash.Session{
		ID:                "sess-dogfood",
		Name:              "Session Dogfood (Self-Host)",
		WorktreeID:        "wt-dogfood",
		ModelTier:         prdash.TierPine,
		SubagentModelTier: prdash.TierBamboo,
		ActiveTask:        "Implementing dynamic entity registration for dogfooding",
	})
	if err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	if sess.ResolvedModel != "claude-opus-4-6" {
		t.Fatalf("unexpected resolved model: %s", sess.ResolvedModel)
	}

	pr, err := svc.UpsertPR(prdash.PullRequest{
		Number:      10,
		Title:       "Support registering and updating worktrees, sessions, and pull requests",
		Description: "Adds UpsertWorktree, UpsertSession, and UpsertPR across Service, HTTP API, and MCP.",
		Branch:      "feat/dynamic-registration",
		BaseBranch:  "main",
		SessionID:   "sess-dogfood",
		WorktreeID:  "wt-dogfood",
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
