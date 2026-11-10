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

func TestReviewPreviewCommandDoesNotAttachPersistedLabels(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "prdash.yaml"), []byte("review:\n  agent: sample-provider\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state.json")
	svc := prdash.NewService(prdash.Options{WorkDir: root, StateFile: state})
	if _, err := svc.UpsertSession(prdash.Session{ID: "sample-reviewer", AgentType: "sample-provider"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpsertPR(prdash.PullRequest{Number: 42, Title: "sample-pr", Branch: "sample-branch"}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := run(t.Context(), []string{"review-preview", "-dir", root, "-state", state, "-pr", "42"}, strings.NewReader(""), &stdout, &stderr)
	if !errors.Is(err, prdash.ErrReviewerUnavailable) {
		t.Fatalf("CLI reviewer error=%v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("CLI invented a connected reviewer: %s", stdout.String())
	}
}
