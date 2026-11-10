package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLICorruptStateFails(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"status", []string{"status"}},
		{"instructions", []string{"instructions"}},
		{"register", []string{"register", "session", "--input", "-"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte("broken JSON"), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			args := append(append([]string{}, tc.args...), "--state", path)
			if err := run(t.Context(), args, strings.NewReader(`{"id":"worker"}`), &stdout, &stderr); err == nil {
				t.Fatalf("corrupt state returned CLI success: %s", stdout.String())
			}
		})
	}
}

func TestCLIDefaultStateFailureDoesNotFallBackToMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", path)
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{"status", "--repo", "example/project"}, strings.NewReader(""), &stdout, &stderr); err == nil {
		t.Fatal("default state error silently disabled persistence")
	}
}
