package prdash_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tenntenn/prdash/prdash"
)

func TestValidatePRWorktreeUsesLiveGit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, change string
		wantErr      bool
	}{
		{"valid branch", "", false},
		{"unknown target commit", "unknown", true},
		{"detached target", "detached", false},
		{"changed commit", "commit", true},
		{"missing worktree", "missing", true},
		{"subdirectory is not worktree root", "subdir", true},
		{"ordinary directory is not a repository", "notgit", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			prepareSBNNRepository(t, dir)
			cmd := exec.CommandContext(t.Context(), "git", "-C", dir, "rev-parse", "HEAD")
			out, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			head := strings.TrimSpace(string(out))
			path := dir
			if tc.change == "unknown" {
				head = ""
			}
			switch tc.change {
			case "detached":
				command := exec.CommandContext(t.Context(), "git", "-C", dir, "checkout", "--detach", head)
				if output, runErr := command.CombinedOutput(); runErr != nil {
					t.Fatalf("detach: %v %s", runErr, output)
				}
			case "commit":
				command := exec.CommandContext(t.Context(), "git", "-C", dir, "-c", "user.name=sample", "-c", "user.email=sample@example.com", "commit", "--allow-empty", "-m", "sample later commit")
				if output, runErr := command.CombinedOutput(); runErr != nil {
					t.Fatalf("commit: %v %s", runErr, output)
				}
			case "missing":
				path = filepath.Join(dir, "sample-missing")
			case "subdir":
				path = filepath.Join(dir, "sample-subdir")
				if mkdirErr := os.Mkdir(path, 0o700); mkdirErr != nil {
					t.Fatal(mkdirErr)
				}
			case "notgit":
				path = t.TempDir()
			}
			svc := prdash.NewService(prdash.Options{})
			if _, registerErr := svc.UpsertWorktree(prdash.Worktree{ID: "sample-wt", Path: path, HeadSHA: head, Status: "clean", Clean: true}); registerErr != nil {
				t.Fatal(registerErr)
			}
			if _, registerErr := svc.UpsertPR(prdash.PullRequest{Number: 42, Title: "sample-pr", WorktreeID: "sample-wt", HeadSHA: head}); registerErr != nil {
				t.Fatal(registerErr)
			}
			got, validationErr := svc.ValidatePRWorktree(t.Context(), 42)
			if tc.wantErr {
				if !errors.Is(validationErr, prdash.ErrReviewWorktreeUnavailable) {
					t.Fatalf("validation error=%v", validationErr)
				}
			} else {
				if validationErr != nil {
					t.Fatal(validationErr)
				}
				if got.HeadSHA != head || got.Detached != (tc.change == "detached") {
					t.Fatalf("validation=%+v", got)
				}
			}
			rec := httptest.NewRecorder()
			handler := prdash.NewHandler(svc)
			server := httptest.NewServer(handler)
			t.Cleanup(server.Close)
			req := httptest.NewRequest(http.MethodPost, server.URL+"/api/prs/42/validate-worktree", bytes.NewReader(nil))
			req.RemoteAddr = "127.0.0.1:12345"
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+acquireHTTPToken(t, server.URL))
			handler.ServeHTTP(rec, req)
			want := http.StatusOK
			if tc.wantErr {
				want = http.StatusConflict
			}
			if rec.Code != want {
				t.Fatalf("HTTP status=%d want=%d body=%s", rec.Code, want, rec.Body.String())
			}
			var mcp bytes.Buffer
			input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"validate_pr_worktree","arguments":{"pr_number":42}}}`
			if rpcErr := prdash.RunMCP(t.Context(), svc, strings.NewReader(input), &mcp); rpcErr != nil {
				t.Fatal(rpcErr)
			}
			var response struct {
				Result struct {
					IsError bool `json:"isError"`
				} `json:"result"`
			}
			if decodeErr := json.Unmarshal(mcp.Bytes(), &response); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if response.Result.IsError != tc.wantErr {
				t.Fatalf("MCP response: %s", mcp.String())
			}

		})
	}
}

func TestSBNNUsesAndValidatesExplicitWorktree(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	prepareSBNNRepository(t, dir)
	binDir := t.TempDir()
	bin := filepath.Join(binDir, "sample-sbnn")
	script := `#!/bin/sh
echo "$PWD" > "$(dirname "$0")/invoked-path"
if [ "$1" = "comments" ]; then echo '[]'; fi
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	client := prdash.NewCLISBNNClient(bin, filepath.Join(binDir, "wrong-directory"))
	if _, err := client.RunReview(t.Context(), prdash.SBNNReviewRequest{PRNumber: 42, BaseBranch: "main", Branch: "feat/sample", WorktreePath: dir}); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(binDir, "invoked-path"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(actual)) != dir {
		t.Fatalf("invoked at %s, want %s", actual, dir)
	}
	_, reviewErr := client.RunReview(t.Context(), prdash.SBNNReviewRequest{PRNumber: 42, WorktreePath: filepath.Join(dir, "missing")})
	_, commentErr := client.SubmitComment(t.Context(), prdash.SBNNCommentRequest{PRNumber: 42, Body: "sample-comment", WorktreePath: filepath.Join(dir, "missing")})
	if !errors.Is(reviewErr, prdash.ErrReviewWorktreeUnavailable) || !errors.Is(commentErr, prdash.ErrReviewWorktreeUnavailable) {
		t.Fatalf("review=%v comment=%v", reviewErr, commentErr)
	}
}
