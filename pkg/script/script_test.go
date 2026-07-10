package script

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestName_EmptyScriptFile(t *testing.T) {
	s := New(zap.NewNop(), "")

	got, err := s.Name()
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestExecute_EmptyScriptFile(t *testing.T) {
	s := New(zap.NewNop(), "")

	if err := s.Execute(context.Background(), "x"); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestName_Success(t *testing.T) {
	path := writeScript(t, "#!/bin/sh\necho hello\n")
	s := New(zap.NewNop(), path)

	got, err := s.Name()
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !strings.Contains(got, "hello") {
		t.Fatalf("expected output to contain %q, got %q", "hello", got)
	}
}

func TestExecute_Success(t *testing.T) {
	path := writeScript(t, "#!/bin/sh\necho hello\n")
	s := New(zap.NewNop(), path)

	if err := s.Execute(context.Background(), "arg"); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestName_NonExistentScript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope")
	s := New(zap.NewNop(), path)

	got, err := s.Name()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestExecute_NonExistentScript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope")
	s := New(zap.NewNop(), path)

	if err := s.Execute(context.Background(), "arg"); err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func TestExecute_ScriptExitsNonZero(t *testing.T) {
	path := writeScript(t, "#!/bin/sh\nexit 1\n")
	s := New(zap.NewNop(), path)

	if err := s.Execute(context.Background(), "arg"); err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func TestName_ScriptExitsNonZero(t *testing.T) {
	path := writeScript(t, "#!/bin/sh\nexit 1\n")
	s := New(zap.NewNop(), path)

	got, err := s.Name()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

// writeScript writes an executable shell script to a temp dir and returns its path.
func writeScript(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "script.sh")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("failed to write script: %v", err)
	}

	return path
}
