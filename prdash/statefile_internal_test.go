package prdash

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicStateRenameFailureCleansTemporaryFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	destination := filepath.Join(root, "state.json")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomicState(destination, []byte(`{"revision":1}`)); err == nil {
		t.Fatal("rename over directory succeeded")
	}
	files, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name() != "state.json" || !files[0].IsDir() {
		t.Fatalf("failed rename left temporary files: %+v", files)
	}
}

func TestStateDetectsNonCooperatingRevisionChange(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	svc := NewService(Options{StateFile: path})
	if err := svc.beginStateMutation(); err != nil {
		t.Fatal(err)
	}
	// A non-cooperating writer changes bytes without incrementing the revision.
	data, err := os.ReadFile(path)
	if err != nil {
		svc.abortStateMutation()
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		svc.abortStateMutation()
		t.Fatal(err)
	}
	svc.controller.ActiveTask = "must not commit"
	if _, err := svc.commitStateMutation(); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("revision conflict=%v", err)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != len(data)+1 {
		t.Fatal("conflicting data was overwritten")
	}
	if svc.controller.ActiveTask == "must not commit" {
		t.Fatal("conflicting update remained in memory")
	}
}
