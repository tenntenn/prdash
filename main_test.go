package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tenntenn/prdash/prdash"
)

func TestRunVersionAndSkill(t *testing.T) {
	t.Parallel()

	t.Run("version flag", func(t *testing.T) {
		t.Parallel()
		var stdout, stderr bytes.Buffer
		if err := run(t.Context(), []string{"-version"}, strings.NewReader(""), &stdout, &stderr); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.HasPrefix(stdout.String(), "prdash ") {
			t.Fatalf("unexpected version output: %q", stdout.String())
		}
	})

	t.Run("skill print and list", func(t *testing.T) {
		t.Parallel()
		var stdout, stderr bytes.Buffer
		if err := run(t.Context(), []string{"skill"}, strings.NewReader(""), &stdout, &stderr); err != nil {
			t.Fatalf("skill print error: %v", err)
		}
		if !strings.Contains(stdout.String(), "name: prdash") {
			t.Fatalf("expected SKILL.md frontmatter, got: %q", stdout.String())
		}

		stdout.Reset()
		if err := run(t.Context(), []string{"skill", "--list"}, strings.NewReader(""), &stdout, &stderr); err != nil {
			t.Fatalf("skill list error: %v", err)
		}
		if strings.TrimSpace(stdout.String()) != "prdash/SKILL.md" {
			t.Fatalf("unexpected skill list output: %q", stdout.String())
		}
	})

	t.Run("skill install and force update", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		var stdout, stderr bytes.Buffer

		if err := run(t.Context(), []string{"skill", "--install", dir}, strings.NewReader(""), &stdout, &stderr); err != nil {
			t.Fatalf("skill install error: %v", err)
		}
		dst := filepath.Join(dir, "prdash", "SKILL.md")
		data, err := os.ReadFile(dst)
		if err != nil {
			t.Fatalf("read installed SKILL.md: %v", err)
		}
		if !strings.Contains(string(data), "name: prdash") {
			t.Fatalf("unexpected installed SKILL.md content: %q", string(data))
		}

		// Re-running without --force must refuse to overwrite.
		if err := run(t.Context(), []string{"skill", "--install", dir}, strings.NewReader(""), &stdout, &stderr); err == nil {
			t.Fatal("expected error when installing over existing file without --force")
		}

		// Re-running with --force must succeed.
		if err := run(t.Context(), []string{"skill", "--install", dir, "--force"}, strings.NewReader(""), &stdout, &stderr); err != nil {
			t.Fatalf("skill force update error: %v", err)
		}
	})
}

func TestCLISubcommandsWorkflow(t *testing.T) {
	t.Parallel()

	statePath := filepath.Join(t.TempDir(), "state.json")
	svc := prdash.NewService(prdash.Options{
		Repository: "example/sample",
		StateFile:  statePath,
		Reviewer:   prdash.NewStubSBNNClient(),
	})
	if _, err := svc.UpsertWorktree(prdash.Worktree{ID: "wt-1", Path: "/tmp/wt-1", Branch: "feat/one", SessionID: "sess-1"}); err != nil {
		t.Fatalf("UpsertWorktree: %v", err)
	}
	if _, err := svc.UpsertSession(prdash.Session{ID: "sess-1", Name: "Worker 1", WorktreeID: "wt-1"}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	if _, err := svc.UpsertPR(prdash.PullRequest{Number: 1, Title: "sample-pr", Branch: "feat/one", SessionID: "sess-1", WorktreeID: "wt-1"}); err != nil {
		t.Fatalf("UpsertPR: %v", err)
	}

	var stdout, stderr bytes.Buffer

	// 1. status
	if err := run(t.Context(), []string{"status", "--repo", "example/sample", "--state", statePath}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("status error: %v", err)
	}
	if !strings.Contains(stdout.String(), "repository: example/sample") {
		t.Fatalf("unexpected status output: %q", stdout.String())
	}

	// 2. send instruction to PR #1
	stdout.Reset()
	if err := run(t.Context(), []string{"send", "--pr", "1", "-m", "sample-cli-instruction", "--state", statePath}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("send error: %v", err)
	}

	// 3. wait returns immediately when an unhandled instruction is present
	stdout.Reset()
	if err := run(t.Context(), []string{"wait", "--session", "sess-1", "--timeout", "200ms", "--state", statePath}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("wait error: %v", err)
	}
	if !strings.Contains(stdout.String(), "sample-cli-instruction") {
		t.Fatalf("expected instruction in wait output, got %q", stdout.String())
	}

	// 4. Legacy report does not acknowledge uncorrelated instructions.
	stdout.Reset()
	if err := run(t.Context(), []string{"report", "--pr", "1", "--status", "waiting_for_human", "-m", "sample-cli-report", "--state", statePath}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("report error: %v", err)
	}

	// 5. Explicitly acknowledge only the returned instructions.
	stdout.Reset()
	if err := run(t.Context(), []string{"instructions", "--session", "sess-1", "--ack", "--state", statePath}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "sample-cli-instruction") {
		t.Fatal("legacy report swallowed pending instruction")
	}
	// 6. wait times out after delivery acknowledgment.
	stdout.Reset()
	err := run(t.Context(), []string{"wait", "--session", "sess-1", "--timeout", "150ms", "--state", statePath}, strings.NewReader(""), &stdout, &stderr)
	if !errors.Is(err, errWaitTimeout) {
		t.Fatalf("expected errWaitTimeout after report, got %v", err)
	}
}

func TestConfigCommand(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "prdash.yaml")
	if err := os.WriteFile(path, []byte("review:\n  agent: sample-cli\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{"config", "-dir", root}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "sample-cli") || !strings.Contains(stdout.String(), "defaulted_fields") {
		t.Fatalf("config output=%s", stdout.String())
	}
	if err := os.WriteFile(path, []byte("version: 99\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"config", "-dir", root}, strings.NewReader(""), &stdout, &stderr); !errors.Is(err, prdash.ErrConfigValidation) {
		t.Fatalf("invalid configuration error=%v", err)
	}
}
