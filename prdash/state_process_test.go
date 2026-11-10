package prdash_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tenntenn/prdash/prdash"
)

// This helper is a separate OS process, with its own Service, locks and memory.
func TestStateProcessHelper(t *testing.T) {
	if os.Getenv("PRDASH_STATE_HELPER") != "1" {
		return
	}
	root := os.Getenv("PRDASH_STATE_ROOT")
	worker := os.Getenv("PRDASH_STATE_WORKER")
	svc := prdash.NewService(prdash.Options{Repository: "example/project", StateFile: filepath.Join(root, "state.json")})
	if err := svc.StateError(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ready-"+worker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitStateFile(t, filepath.Join(root, "start"))
	if os.Getenv("PRDASH_STATE_MODE") == "claim" {
		_, err := svc.ClaimInstruction(prdash.InstructionClaimRequest{SessionID: "controller", InstructionID: os.Getenv("PRDASH_INSTRUCTION_ID"), ConsumerID: worker})
		if err == nil {
			if err := os.WriteFile(filepath.Join(root, "claimed-"+worker), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(err, prdash.ErrInstructionConflict) {
			t.Fatal(err)
		}
		return
	}
	for i := range 10 {
		if _, err := svc.SendControllerInstruction(fmt.Sprintf("worker-%s-message-%d", worker, i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.UpsertSession(prdash.Session{ID: "worker-" + worker}); err != nil {
		t.Fatal(err)
	}
}

func waitStateFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", path)
		case <-ticker.C:
		}
	}
}

func runStateProcesses(t *testing.T, root, mode, instruction string) {
	t.Helper()
	const consumers = 8
	commands := make([]*exec.Cmd, consumers)
	outputs := make([]bytes.Buffer, consumers)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for i := range consumers {
		commands[i] = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStateProcessHelper$")
		commands[i].Env = append(os.Environ(), "PRDASH_STATE_HELPER=1", "PRDASH_STATE_ROOT="+root, "PRDASH_STATE_WORKER="+strconv.Itoa(i), "PRDASH_STATE_MODE="+mode, "PRDASH_INSTRUCTION_ID="+instruction)
		commands[i].Stdout = &outputs[i]
		commands[i].Stderr = &outputs[i]
		if err := commands[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for i := range consumers {
		waitStateFile(t, filepath.Join(root, "ready-"+strconv.Itoa(i)))
	}
	if err := os.WriteFile(filepath.Join(root, "start"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for i, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Errorf("process %d: %v\n%s", i, err, outputs[i].String())
		}
	}
}

func TestConcurrentStateProcessesPreserveHistory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	runStateProcesses(t, root, "updates", "")
	svc := prdash.NewService(prdash.Options{StateFile: filepath.Join(root, "state.json")})
	if err := svc.StateError(); err != nil {
		t.Fatal(err)
	}
	snap := svc.Snapshot()
	if len(snap.ControllerSession.Messages) != 80 || len(snap.Sessions) != 8 {
		t.Fatalf("lost updates: messages=%d sessions=%d", len(snap.ControllerSession.Messages), len(snap.Sessions))
	}
	seen := make(map[string]bool)
	for _, msg := range snap.ControllerSession.Messages {
		if seen[msg.ID] {
			t.Fatalf("duplicate ID %s", msg.ID)
		}
		seen[msg.ID] = true
	}
	if snap.StatePath != filepath.Join(root, "state.json") || snap.StateError != "" {
		t.Fatalf("state diagnostics: %+v", snap)
	}
	files, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file.Name(), ".tmp") {
			t.Fatalf("temporary file leaked: %s", file.Name())
		}
	}
}

func TestConcurrentStateProcessesClaimOnce(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	svc := prdash.NewService(prdash.Options{StateFile: filepath.Join(root, "state.json")})
	if _, err := svc.SendControllerInstruction("claim once across processes"); err != nil {
		t.Fatal(err)
	}
	msg := svc.PendingInstructions("controller")[0]
	runStateProcesses(t, root, "claim", msg.ID)
	files, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, file := range files {
		if strings.HasPrefix(file.Name(), "claimed-") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("successful process claims = %d", count)
	}
	current, err := svc.GetInstruction("controller", msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ExecutionState != prdash.InstructionRunning || current.Attempt != 1 {
		t.Fatalf("claim not persisted: %+v", current)
	}
}

func TestStateErrorsDoNotReportDurableSuccess(t *testing.T) {
	cases := []struct {
		name    string
		corrupt bool
	}{{"corrupt state", true}, {"invalid parent", false}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			path := filepath.Join(root, "state.json")
			if tc.corrupt {
				if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				parent := filepath.Join(root, "file")
				if err := os.WriteFile(parent, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(parent, "state.json")
			}
			svc := prdash.NewService(prdash.Options{StateFile: path})
			if svc.StateError() == nil {
				t.Fatal("startup failure suppressed")
			}
			if _, err := svc.SendControllerInstruction("must fail"); err == nil {
				t.Fatal("mutation returned success")
			}
			if tc.corrupt {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != "{broken" {
					t.Fatal("corrupt state overwritten")
				}
			}
		})
	}
}

func TestStateSerializationFailureRollsBack(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	svc := prdash.NewService(prdash.Options{StateFile: path})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.UpsertPR(prdash.PullRequest{Number: 1, Title: "bad timestamp", Thread: []prdash.ThreadMessage{{CreatedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}}})
	if !errors.Is(err, prdash.ErrStatePersistence) {
		t.Fatalf("serialization failure=%v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed serialization changed disk")
	}
	if _, err := svc.GetPR(1); !errors.Is(err, prdash.ErrPRNotFound) {
		t.Fatalf("failed update remained in memory: %v", err)
	}
}

func TestStateCorruptionIsVisibleThroughHTTPAndMCP(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "state.json")
	svc := prdash.NewService(prdash.Options{StateFile: path})
	if err := os.WriteFile(path, []byte("not JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := prdash.NewHandler(svc)
	rec := instructionHTTP(t, handler, "GET", "/api/state", "")
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "state_error") {
		t.Fatalf("HTTP hid corrupt state: %d %s", rec.Code, rec.Body.String())
	}
	rec = instructionHTTP(t, handler, "POST", "/api/sessions", `{"id":"new-session"}`)
	if rec.Code < 400 {
		t.Fatalf("HTTP mutation succeeded on corrupt state: %d %s", rec.Code, rec.Body.String())
	}
	var output strings.Builder
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_dashboard","arguments":{}}}` + "\n"
	if err := prdash.RunMCP(t.Context(), svc, strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"isError":true`) {
		t.Fatalf("MCP hid corrupt state: %s", output.String())
	}
}

func TestStatePathResolvesSymlinkAliases(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	alias := filepath.Join(root, "alias")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	first := prdash.NewService(prdash.Options{StateFile: filepath.Join(realDir, "state.json")})
	second := prdash.NewService(prdash.Options{StateFile: filepath.Join(alias, "state.json")})
	if first.StatePath() != second.StatePath() {
		t.Fatalf("aliases use different lock targets: %s %s", first.StatePath(), second.StatePath())
	}
	if _, err := first.SendControllerInstruction("first"); err != nil {
		t.Fatal(err)
	}
	if _, err := second.SendControllerInstruction("second"); err != nil {
		t.Fatal(err)
	}
	if len(first.Snapshot().ControllerSession.Messages) != 2 {
		t.Fatal("alias lost update")
	}
}
