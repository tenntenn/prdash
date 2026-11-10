package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tenntenn/prdash/prdash"
)

func TestRegistrationAssignmentReviewCLI(t *testing.T) {
	t.Parallel()
	state := filepath.Join(t.TempDir(), "state.json")
	invoke := func(input string, args ...string) []byte {
		t.Helper()
		args = append(args, "--repo", "example/sample", "--state", state, "--json")
		var out, stderr bytes.Buffer
		if err := run(t.Context(), args, strings.NewReader(input), &out, &stderr); err != nil {
			t.Fatalf("%v: %v (%s)", args, err, stderr.String())
		}
		if !json.Valid(out.Bytes()) {
			t.Fatalf("invalid JSON: %s", out.String())
		}
		return out.Bytes()
	}
	invoke(`{"id":"wt","path":"/tmp/example-worktree","branch":"feature"}`, "register", "worktree")
	sessionFile := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(sessionFile, []byte(`{"id":"worker","name":"Worker"}`), 0600); err != nil {
		t.Fatal(err)
	}
	invoke("", "register", "session", "--input", sessionFile)
	invoke(`{"number":35,"title":"Original","ci_status":"failing","thread":[{"id":"history","text":"keep me"}]}`, "register", "pr")
	invoke(`{"number":35,"title":"Updated","ci_status":"passing"}`, "register", "pr")
	var assigned prdash.PullRequest
	if err := json.Unmarshal(invoke("", "assign", "--pr", "35", "--worktree", "wt", "--session", "worker"), &assigned); err != nil {
		t.Fatal(err)
	}
	if assigned.Title != "Updated" || assigned.CIStatus != "passing" || assigned.WorktreeID != "wt" || assigned.SessionID != "worker" || len(assigned.Thread) != 1 || assigned.Thread[0].Text != "keep me" {
		t.Fatalf("assignment lost state: %+v", assigned)
	}
	before, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if err := run(t.Context(), []string{"assign", "--repo", "example/sample", "--state", state, "--pr", "35", "--worktree", "missing", "--session", "worker"}, strings.NewReader(""), &out, &stderr); err == nil {
		t.Fatal("missing destination accepted")
	}
	after, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("invalid assignment mutated state")
	}
	if err := run(t.Context(), []string{"register", "session", "--repo", "other/repo", "--state", state}, strings.NewReader(`{"id":"wrong"}`), &out, &stderr); err == nil {
		t.Fatal("repository mismatch accepted")
	}
	invoke("Fix the review comments\nand run tests.", "review", "address", "--pr", "35", "--stdin")
	svc := prdash.NewService(prdash.Options{Repository: "example/sample", StateFile: state})
	pending := svc.PendingInstructions("worker")
	if len(pending) != 1 || !strings.Contains(pending[0].Text, "Fix the review comments\nand run tests.") || pending[0].PRNumber != 35 {
		t.Fatalf("unexpected instruction: %+v", pending)
	}
}

func TestMutationCLIValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		args  []string
		input string
	}{
		{"missing entity", []string{"register"}, ""},
		{"unknown entity", []string{"register", "unknown"}, "{}"},
		{"malformed", []string{"register", "session"}, "{"},
		{"unknown field", []string{"register", "session"}, `{"id":"s","typo":true}`},
		{"trailing object", []string{"register", "session"}, `{"id":"s"} {}`},
		{"missing assignment", []string{"assign", "--pr", "35"}, ""},
		{"missing review action", []string{"review"}, ""},
		{"unknown review action", []string{"review", "unknown"}, ""},
		{"missing pr", []string{"review", "request"}, ""},
		{"conflicting message", []string{"review", "address", "--pr", "35", "-m", "text", "--stdin"}, "text"},
		{"empty message", []string{"review", "address", "--pr", "35", "--stdin"}, "  "},
		{"positional", []string{"assign", "unexpected"}, ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out, stderr bytes.Buffer
			if err := run(t.Context(), tt.args, strings.NewReader(tt.input), &out, &stderr); err == nil {
				t.Fatal("expected input error")
			}
			if out.Len() != 0 {
				t.Fatalf("unexpected success output: %s", out.String())
			}
		})
	}
}

func TestReviewRequestCLI(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	git("init", "-b", "main")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "base")
	git("switch", "-c", "feature")
	if err := os.WriteFile(filepath.Join(dir, "sample.txt"), []byte("sample"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "sample.txt")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "feature")
	bin := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = "comments" ]; then
 printf '[{"id":"c1","path":"sample.txt","startLine":1,"body":"Check sample","resolved":false}]'
else
 cat >/dev/null
 printf '{"group":"pr-35","url":"http://localhost:6280/pr-35"}'
fi
`
	if err := os.WriteFile(filepath.Join(bin, "sbnn"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	state := filepath.Join(t.TempDir(), "state.json")
	svc := prdash.NewService(prdash.Options{Repository: "example/sample", StateFile: state})
	if _, err := svc.UpsertPR(prdash.PullRequest{Number: 35, Title: "Review me", Branch: "feature", BaseBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if err := run(t.Context(), []string{"review", "request", "--repo", "example/sample", "--dir", dir, "--state", state, "--pr", "35", "-m", "Check correctness", "--json"}, strings.NewReader(""), &out, &stderr); err != nil {
		t.Fatal(err)
	}
	var pr prdash.PullRequest
	if err := json.Unmarshal(out.Bytes(), &pr); err != nil {
		t.Fatal(err)
	}
	if pr.ReviewStatus != "changes_requested" {
		t.Fatalf("unexpected review: %+v", pr)
	}
	if !strings.Contains(out.String(), "Check sample") {
		t.Fatalf("missing reviewer finding: %s", out.String())
	}
}

func TestMutationCLIRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prdash.yaml"), []byte("version: 99\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		args  []string
		input string
	}{
		{"register", []string{"register", "session"}, `{"id":"worker"}`},
		{"assign", []string{"assign", "--pr", "35", "--worktree", "wt", "--session", "worker"}, ""},
		{"review", []string{"review", "address", "--pr", "35"}, ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			args := append(tt.args, "--repo", "example/sample", "--dir", dir, "--state", "off")
			var out, stderr bytes.Buffer
			err := run(t.Context(), args, strings.NewReader(tt.input), &out, &stderr)
			if err == nil || !strings.Contains(err.Error(), "version") {
				t.Fatalf("expected config error, got %v", err)
			}
		})
	}
}
