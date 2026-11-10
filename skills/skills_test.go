package skills_test

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/tenntenn/prdash/skills"
)

func TestEmbeddedSkill(t *testing.T) {
	t.Parallel()

	md, err := skills.Markdown()
	if err != nil {
		t.Fatalf("Markdown() failed: %v", err)
	}
	if !strings.Contains(string(md), "name: prdash") {
		t.Fatalf("expected SKILL.md frontmatter, got: %q", string(md))
	}

	entries, err := fs.ReadDir(skills.FS(), skills.Name)
	if err != nil {
		t.Fatalf("ReadDir(%q) failed: %v", skills.Name, err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one file in embedded skill directory")
	}
}
