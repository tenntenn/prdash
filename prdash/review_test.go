package prdash_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tenntenn/prdash/prdash"
)

type sampleReviewRuntime struct {
	requests []prdash.SBNNReviewRequest
	applied  string
	err      error
}

func (r *sampleReviewRuntime) RunReview(_ context.Context, req prdash.SBNNReviewRequest) (prdash.SBNNReviewResult, error) {
	r.requests = append(r.requests, req)
	return prdash.SBNNReviewResult{AppliedModel: r.applied, Summary: "sample-result", Findings: []string{"sample-finding"}, CreatedAt: time.Now().UTC()}, r.err
}

func reviewService(t *testing.T, role string, runtime *sampleReviewRuntime) (*prdash.Service, string) {
	t.Helper()
	root := configRepo(t)
	if err := os.WriteFile(filepath.Join(root, "review.md"), []byte("sample-instructions"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeRepoConfig(t, root, "review:\n  agent: sample-provider\n  session: "+role+"\n  model: sample-model\n  instructions_file: review.md\n")
	bindings := map[string]prdash.ReviewRuntimeBinding{}
	id := "sample-reviewer"
	if role == "working" {
		id = "sample-owner"
	}
	if role == "controller" {
		id = "sess-controller"
	}
	bindings[id] = prdash.ReviewRuntimeBinding{Provider: "sample-provider", Runtime: runtime, SupportedModels: []string{"sample-model"}, Dedicated: true}
	svc := prdash.NewService(prdash.Options{WorkDir: root, StateFile: filepath.Join(root, "state.json"), ReviewRuntimes: bindings})
	for _, id := range []string{"sample-owner", "sample-reviewer"} {
		if _, err := svc.UpsertSession(prdash.Session{ID: id, AgentType: "sample-provider"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.UpsertWorktree(prdash.Worktree{ID: "sample-wt", Path: root, Branch: "sample-branch", HeadSHA: strings.Repeat("a", 40), SessionID: "sample-owner"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpsertPR(prdash.PullRequest{Number: 42, Title: "sample-pr", Branch: "sample-branch", WorktreeID: "sample-wt", SessionID: "sample-owner", Thread: []prdash.ThreadMessage{{ID: "existing-comment", Kind: "review_comment", Text: "sample-outstanding"}}}); err != nil {
		t.Fatal(err)
	}
	return svc, root
}

func TestReviewPortableRoles(t *testing.T) {
	t.Parallel()
	cases := []struct{ role, id string }{{"working", "sample-owner"}, {"separate", "sample-reviewer"}, {"dedicated", "sample-reviewer"}, {"controller", "sess-controller"}}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			t.Parallel()
			runtime := &sampleReviewRuntime{applied: "sample-model"}
			svc, root := reviewService(t, tc.role, runtime)
			plan, err := svc.PreviewReview(t.Context(), 42, "sample-focus")
			if err != nil {
				t.Fatal(err)
			}
			if len(runtime.requests) != 0 || plan.AppliedModel != "" || plan.ActualReviewer != "" {
				t.Fatal("preview executed or invented runtime facts")
			}
			if plan.ReviewerSessionID != tc.id || plan.Instructions != "sample-instructions" || plan.Config.SourcePath != filepath.Join(root, "prdash.yaml") {
				t.Fatalf("preview=%+v", plan)
			}
			pr, err := svc.RequestReviewWithPreview(t.Context(), 42, "sample-focus", plan.PreviewKey)
			if err != nil {
				t.Fatal(err)
			}
			if pr.SessionID != "sample-owner" || pr.WorktreeID != "sample-wt" || pr.Thread[0].ID != "existing-comment" {
				t.Fatal("review replaced owner/worktree/comments")
			}
			if len(runtime.requests) != 1 {
				t.Fatalf("requests=%d", len(runtime.requests))
			}
			req := runtime.requests[0]
			if req.Execution.TargetCommit != strings.Repeat("a", 40) || req.Execution.RequestID == "" || req.Execution.RequestedModel != "sample-model" {
				t.Fatalf("dispatch=%+v", req)
			}
			event := pr.Thread[1].ReviewExecution
			if event == nil || event.ActualReviewer != tc.id || event.AppliedModel != "sample-model" || event.Status != "completed" {
				t.Fatalf("audit=%+v", event)
			}
			// Restoring state cannot turn a persisted label into a live connection.
			restored := prdash.NewService(prdash.Options{WorkDir: root, StateFile: filepath.Join(root, "state.json")})
			if _, err := restored.PreviewReview(t.Context(), 42, ""); !errors.Is(err, prdash.ErrReviewerUnavailable) {
				t.Fatalf("persisted reviewer connected: %v", err)
			}
			restoredPR, err := restored.GetPR(42)
			if err != nil {
				t.Fatal(err)
			}
			if restoredPR.Thread[1].ReviewExecution.RequestID != event.RequestID {
				t.Fatal("execution audit not persisted")
			}
		})
	}
}

func TestReviewerRejectsUnavailableAndChangedPolicy(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, config string }{{"provider", "review:\n  agent: other-provider\n"}, {"unsupported model", "review:\n  agent: sample-provider\n  model: other-model\n"}, {"sbnn instructions", "review:\n  instructions_file: review.md\n"}, {"working role", "review:\n  agent: sample-provider\n  session: working\n"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runtime := &sampleReviewRuntime{applied: "sample-model"}
			svc, root := reviewService(t, "separate", runtime)
			writeRepoConfig(t, root, tc.config)
			if _, err := svc.ReloadConfig(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.RequestReview(t.Context(), 42, ""); !errors.Is(err, prdash.ErrReviewerUnavailable) {
				t.Fatalf("dispatch error=%v", err)
			}
			if len(runtime.requests) != 0 {
				t.Fatal("unavailable reviewer was dispatched")
			}
		})
	}
	t.Run("stale preview", func(t *testing.T) {
		runtime := &sampleReviewRuntime{applied: "sample-model"}
		svc, root := reviewService(t, "separate", runtime)
		plan, err := svc.PreviewReview(t.Context(), 42, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "review.md"), []byte("sample-changed"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ReloadConfig(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.RequestReviewWithPreview(t.Context(), 42, "", plan.PreviewKey); !errors.Is(err, prdash.ErrReviewerUnavailable) {
			t.Fatalf("stale preview error=%v", err)
		}
		if len(runtime.requests) != 0 {
			t.Fatal("stale preview dispatched")
		}
	})
}

func TestReviewFailureAudit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, applied string
		failure       error
	}{{name: "wrong model", applied: "other-model"}, {name: "execution failure", failure: errors.New("sample-offline")}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runtime := &sampleReviewRuntime{applied: tc.applied, err: tc.failure}
			svc, _ := reviewService(t, "separate", runtime)
			if _, err := svc.RequestReview(t.Context(), 42, ""); err == nil {
				t.Fatal("failed runtime accepted")
			}
			pr, err := svc.GetPR(42)
			if err != nil {
				t.Fatal(err)
			}
			event := pr.Thread[1].ReviewExecution
			if event == nil || event.Status != "failed" || event.Error == "" {
				t.Fatalf("failed audit=%+v", event)
			}
			if tc.failure == nil && event.AppliedModel != tc.applied {
				t.Fatal("failed model confirmation hid the model actually used")
			}
			if len(pr.Thread) != 2 {
				t.Fatal("failed execution added a successful review comment")
			}
		})
	}
}

func TestReviewPreviewHTTPAndMCP(t *testing.T) {
	t.Parallel()
	runtime := &sampleReviewRuntime{applied: "sample-model"}
	svc, _ := reviewService(t, "separate", runtime)
	rec := httptest.NewRecorder()
	prdash.NewHandler(svc).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/prs/42/review-preview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status=%d %s", rec.Code, rec.Body.String())
	}
	var plan prdash.ReviewExecution
	if err := json.Unmarshal(rec.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"preview_review","arguments":{"pr_number":42}}}` + "\n")
	if err := prdash.RunMCP(t.Context(), svc, input, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), plan.PreviewKey) || len(runtime.requests) != 0 {
		t.Fatalf("preview MCP=%s", output.String())
	}
}

func TestAmbiguousAndDisconnectedReviewers(t *testing.T) {
	t.Parallel()
	runtime := &sampleReviewRuntime{applied: "sample-model"}
	svc, root := reviewService(t, "separate", runtime)
	if _, err := svc.UpsertSession(prdash.Session{ID: "sample-other", AgentType: "sample-provider"}); err != nil {
		t.Fatal(err)
	}
	binding := prdash.ReviewRuntimeBinding{Provider: "sample-provider", Runtime: runtime, SupportedModels: []string{"sample-model"}}
	ambiguous := prdash.NewService(prdash.Options{WorkDir: root, StateFile: filepath.Join(root, "state.json"), ReviewRuntimes: map[string]prdash.ReviewRuntimeBinding{"sample-reviewer": binding, "sample-other": binding}})
	if _, err := ambiguous.PreviewReview(t.Context(), 42, ""); !errors.Is(err, prdash.ErrReviewerUnavailable) {
		t.Fatalf("ambiguous review error=%v", err)
	}
	if _, err := svc.UpsertSession(prdash.Session{ID: "sample-reviewer", AgentType: "sample-provider", Status: "disconnected"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PreviewReview(t.Context(), 42, ""); !errors.Is(err, prdash.ErrReviewerUnavailable) {
		t.Fatalf("disconnected review error=%v", err)
	}
	if len(runtime.requests) != 0 {
		t.Fatal("unavailable reviewer was invoked")
	}
}

func TestReviewPreviewRejectsChangedModelPreset(t *testing.T) {
	t.Parallel()
	runtime := &sampleReviewRuntime{applied: "sample-model"}
	_, root := reviewService(t, "separate", runtime)
	writeRepoConfig(t, root, "review:\n  agent: sample-provider\n  model_tier: bamboo\n")
	svc := prdash.NewService(prdash.Options{WorkDir: root, StateFile: filepath.Join(root, "state.json"), ReviewRuntimes: map[string]prdash.ReviewRuntimeBinding{"sample-reviewer": {Provider: "sample-provider", Runtime: runtime, SupportedModels: []string{"sample-model", "sample-next"}}}})
	if _, err := svc.UpdateModelConfig(prdash.ModelPresetConfig{Bamboo: "sample-model"}); err != nil {
		t.Fatal(err)
	}
	plan, err := svc.PreviewReview(t.Context(), 42, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateModelConfig(prdash.ModelPresetConfig{Bamboo: "sample-next"}); err != nil {
		t.Fatal(err)
	}
	if svc.EffectiveConfig().Revision != plan.Config.Revision {
		t.Fatal("test must change only the runtime model preset")
	}
	if _, err := svc.RequestReviewWithPreview(t.Context(), 42, "", plan.PreviewKey); !errors.Is(err, prdash.ErrReviewerUnavailable) {
		t.Fatalf("changed model preview error=%v", err)
	}
	if len(runtime.requests) != 0 {
		t.Fatal("stale model choice dispatched")
	}
}
