package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
