package prdash_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tenntenn/prdash/prdash"
)

func configRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}
func writeRepoConfig(t *testing.T, root, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "prdash.yaml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryConfigDefaultsAndRoot(t *testing.T) {
	t.Parallel()
	root := configRepo(t)
	nested := filepath.Join(root, "sample", "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRepoConfig(t, nested, "version: 99\n")
	cfg, err := prdash.LoadRepoConfig(t.Context(), nested)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RepoRoot != root || cfg.Source != "defaults" || cfg.Config.Version != 1 || cfg.Config.ReviewResponse.Push != "confirm" || !slices.Contains(cfg.DefaultedFields, "review.agent") {
		t.Fatalf("unexpected default config: %+v", cfg)
	}
	again, err := prdash.LoadRepoConfig(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if again.Revision != cfg.Revision {
		t.Fatal("identical policy changed revision")
	}
}

func TestRepositoryConfigSchema(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, content, field string
		line                 int
	}{
		{"version", "version: 2\n", "version", 1},
		{"unknown", "review:\n  typo: sample\n", "review.typo", 2},
		{"duplicate", "version: 1\nversion: 1\n", "version", 2},
		{"wrong type", "version: '1'\n", "version", 1},
		{"null section", "review: null\n", "review", 1},
		{"anchor", "review: &sample\n  agent: sbnn\n", "review", 1},
		{"bad session", "review:\n  session: missing\n", "review.session", 2},
		{"model conflict", "review:\n  model: sample-model\n  model_tier: pine\n", "review.model_tier", 3},
		{"gate", "review_response:\n  push: yes\n", "review_response.push", 2},
		{"contradiction", "review_response:\n  test: off\n  verify: [sample-command]\n", "review_response.verify", 3},
		{"push contradiction", "review_response:\n  commit: off\n  push: auto\n", "review_response.push", 3},
		{"negative rounds", "review_response:\n  rereview:\n    max_rounds: -1\n", "review_response.rereview.max_rounds", 3},
		{"list value", "review_response:\n  verify: [true]\n", "review_response.verify", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := configRepo(t)
			writeRepoConfig(t, root, tc.content)
			_, err := prdash.LoadRepoConfig(t.Context(), root)
			if !errors.Is(err, prdash.ErrConfigValidation) {
				t.Fatalf("error=%v", err)
			}
			configErr, ok := errors.AsType[*prdash.ConfigError](err)
			if !ok || configErr.Field != tc.field || configErr.Line != tc.line || configErr.File != filepath.Join(root, "prdash.yaml") || configErr.Reason == "" {
				t.Fatalf("wrong diagnostic: %+v", configErr)
			}
		})
	}
}

func TestRepositoryConfigSyntaxAndDocuments(t *testing.T) {
	t.Parallel()
	cases := []string{"review: [\n", "version: 1\n---\nversion: 1\n", "- sample\n", "review:\n  <<: {agent: sample}\n"}
	for _, content := range cases {
		t.Run(content, func(t *testing.T) {
			t.Parallel()
			root := configRepo(t)
			writeRepoConfig(t, root, content)
			if _, err := prdash.LoadRepoConfig(t.Context(), root); !errors.Is(err, prdash.ErrConfigValidation) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestRepositoryConfigInstructionsAndIsolation(t *testing.T) {
	t.Parallel()
	root := configRepo(t)
	other := configRepo(t)
	if err := os.WriteFile(filepath.Join(root, "sample.md"), []byte("sample instructions"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeRepoConfig(t, root, "review:\n  agent: 'sample # agent'\n  instructions_file: sample.md\nreview_response:\n  verify:\n    - |\n      echo 'sample: value'\n")
	cfg, err := prdash.LoadRepoConfig(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Config.Review.Agent != "sample # agent" || cfg.Instructions["review.instructions_file"] != "sample instructions" || cfg.Config.ReviewResponse.Verify[0] != "echo 'sample: value'\n" || slices.Contains(cfg.DefaultedFields, "review.agent") {
		t.Fatalf("config=%+v", cfg)
	}
	isolated, err := prdash.LoadRepoConfig(t.Context(), other)
	if err != nil {
		t.Fatal(err)
	}
	if isolated.Config.Review.Agent != "sbnn" || len(isolated.Instructions) != 0 {
		t.Fatal("configuration leaked between repositories")
	}
	if err := os.Symlink(filepath.Join(other, "outside.md"), filepath.Join(root, "linked.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "outside.md"), []byte("sample outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"missing.md", "linked.md", filepath.Join(other, "outside.md")} {
		writeRepoConfig(t, root, "review:\n  instructions_file: "+path+"\n")
		if _, loadErr := prdash.LoadRepoConfig(t.Context(), root); !errors.Is(loadErr, prdash.ErrConfigValidation) {
			t.Fatalf("path=%s error=%v", path, loadErr)
		}
	}
}

func TestConfigReloadPreservesQueuedPolicy(t *testing.T) {
	t.Parallel()
	root := configRepo(t)
	state := filepath.Join(t.TempDir(), "state.json")
	writeRepoConfig(t, root, "review:\n  agent: sample-first\nreview_response:\n  verify: [sample-first]\n")
	svc := prdash.NewService(prdash.Options{WorkDir: root, StateFile: state})
	if err := svc.ConfigLoadError(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SendControllerInstruction("sample-first-request"); err != nil {
		t.Fatal(err)
	}
	before := svc.PendingInstructions("controller")[0].AppliedConfig
	if before == nil {
		t.Fatal("missing request policy")
	}
	writeRepoConfig(t, root, "review:\n  agent: sample-second\n")
	if svc.EffectiveConfig().Config.Review.Agent != "sample-first" {
		t.Fatal("config changed without explicit reload")
	}
	after, err := svc.ReloadConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after.Config.Review.Agent != "sample-second" || after.Revision == before.Revision {
		t.Fatalf("reload=%+v", after)
	}
	pending := svc.PendingInstructions("controller")
	pending[0].AppliedConfig.Config.ReviewResponse.Verify[0] = "sample-mutated"
	if svc.PendingInstructions("controller")[0].AppliedConfig.Config.ReviewResponse.Verify[0] != "sample-first" {
		t.Fatal("snapshot mutation reached service")
	}
	writeRepoConfig(t, root, "version: 99\n")
	if _, reloadErr := svc.ReloadConfig(t.Context()); reloadErr == nil {
		t.Fatal("invalid reload accepted")
	}
	if svc.EffectiveConfig().Revision != after.Revision {
		t.Fatal("bad reload replaced last good config")
	}
	if _, sendErr := svc.SendControllerInstruction("sample-blocked"); !errors.Is(sendErr, prdash.ErrConfigValidation) {
		t.Fatalf("dispatch error=%v", sendErr)
	}
	restored := prdash.NewService(prdash.Options{WorkDir: root, StateFile: state})
	if restored.PendingInstructions("controller")[0].AppliedConfig.Revision != before.Revision {
		t.Fatal("request policy lost on restart")
	}
}

func TestConfigWebAndMCP(t *testing.T) {
	t.Parallel()
	root := configRepo(t)
	writeRepoConfig(t, root, "review:\n  agent: sample-agent\n")
	svc := prdash.NewService(prdash.Options{WorkDir: root})
	rec := authenticatedHTTP(t, prdash.NewHandler(svc), http.MethodGet, "/api/state", "")
	var snap prdash.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Config.Config.Review.Agent != "sample-agent" {
		t.Fatalf("web config=%+v", snap.Config)
	}
	var output bytes.Buffer
	input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_config","arguments":{}}}` + "\n")
	if err := prdash.RunMCP(t.Context(), svc, input, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "sample-agent") {
		t.Fatalf("MCP response=%s", output.String())
	}
}

type reloadingConfigReviewer struct {
	prdash.StubSBNNClient
	service  *prdash.Service
	root     string
	captured prdash.EffectiveConfig
}

func (r *reloadingConfigReviewer) RunReview(ctx context.Context, req prdash.SBNNReviewRequest) (prdash.SBNNReviewResult, error) {
	r.captured = req.AppliedConfig
	if err := os.WriteFile(filepath.Join(r.root, "prdash.yaml"), []byte("review:\n  agent: sample-second\n"), 0o600); err != nil {
		return prdash.SBNNReviewResult{}, err
	}
	if _, err := r.service.ReloadConfig(ctx); err != nil {
		return prdash.SBNNReviewResult{}, err
	}
	return r.StubSBNNClient.RunReview(ctx, req)
}
func TestInFlightReviewKeepsConfiguration(t *testing.T) {
	t.Parallel()
	root := configRepo(t)
	writeRepoConfig(t, root, "review:\n  agent: sample-first\n")
	reviewer := &reloadingConfigReviewer{root: root}
	svc := prdash.NewService(prdash.Options{WorkDir: root, Reviewer: reviewer})
	reviewer.service = svc
	if _, err := svc.UpsertPR(prdash.PullRequest{Number: 42, Title: "sample-pr", Branch: "sample-branch"}); err != nil {
		t.Fatal(err)
	}
	pr, err := svc.RequestReview(t.Context(), 42, "")
	if err != nil {
		t.Fatal(err)
	}
	if reviewer.captured.Config.Review.Agent != "sample-first" || svc.EffectiveConfig().Config.Review.Agent != "sample-second" {
		t.Fatal("in-flight config was not frozen")
	}
	for _, msg := range pr.Thread {
		if msg.Kind == "review_request" && (msg.AppliedConfig == nil || msg.AppliedConfig.Revision != reviewer.captured.Revision) {
			t.Fatal("review audit used reloaded config")
		}
	}
}

func TestConfigReloadEndpoints(t *testing.T) {
	t.Parallel()
	root := configRepo(t)
	svc := prdash.NewService(prdash.Options{WorkDir: root})
	writeRepoConfig(t, root, "review:\n  agent: sample-http\n")
	rec := authenticatedHTTP(t, prdash.NewHandler(svc), http.MethodPost, "/api/config/reload", `{}`)
	if rec.Code != http.StatusOK || svc.EffectiveConfig().Config.Review.Agent != "sample-http" {
		t.Fatalf("HTTP reload: %d %s", rec.Code, rec.Body.String())
	}
	writeRepoConfig(t, root, "review:\n  agent: sample-mcp\n")
	var output bytes.Buffer
	input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"reload_config","arguments":{}}}` + "\n")
	if err := prdash.RunMCP(t.Context(), svc, input, &output); err != nil {
		t.Fatal(err)
	}
	if svc.EffectiveConfig().Config.Review.Agent != "sample-mcp" || !strings.Contains(output.String(), "sample-mcp") {
		t.Fatalf("MCP reload=%s", output.String())
	}
	writeRepoConfig(t, root, "unknown: value\n")
	rec = authenticatedHTTP(t, prdash.NewHandler(svc), http.MethodPost, "/api/config/reload", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad reload status=%d", rec.Code)
	}
}

func TestLinkedWorktreeConfigRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: sample-worktree-metadata\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "nested")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRepoConfig(t, root, "review:\n  agent: sample-worktree\n")
	cfg, err := prdash.LoadRepoConfig(t.Context(), sub)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RepoRoot != root || cfg.Config.Review.Agent != "sample-worktree" {
		t.Fatalf("worktree configuration=%+v", cfg)
	}
}
