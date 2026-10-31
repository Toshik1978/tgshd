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

	got, err := s.Name(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestExecute_EmptyScriptFile(t *testing.T) {
	s := New(zap.NewNop(), "")

	if err := s.Execute(context.Background(), "x", 0); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestName_Success(t *testing.T) {
	path := writeScript(t, "#!/bin/sh\necho hello\n")
	s := New(zap.NewNop(), path)

	got, err := s.Name(context.Background())
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

	if err := s.Execute(context.Background(), "arg", 0); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestName_NonExistentScript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope")
	s := New(zap.NewNop(), path)

	got, err := s.Name(context.Background())
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

	if err := s.Execute(context.Background(), "arg", 0); err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func TestExecute_ScriptExitsNonZero(t *testing.T) {
	path := writeScript(t, "#!/bin/sh\nexit 1\n")
	s := New(zap.NewNop(), path)

	if err := s.Execute(context.Background(), "arg", 0); err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func TestName_ScriptExitsNonZero(t *testing.T) {
	path := writeScript(t, "#!/bin/sh\nexit 1\n")
	s := New(zap.NewNop(), path)

	got, err := s.Name(context.Background())
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestName_ContextCancelled(t *testing.T) {
	path := writeScript(t, "#!/bin/sh\necho hello\n")
	s := New(zap.NewNop(), path)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.Name(ctx); err == nil {
		t.Fatal("expected an error when the context is already cancelled")
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

func TestExecute_ThreadIDEnv(t *testing.T) {
	t.Setenv("TGSHD_TEST_INHERIT", "kept")

	tests := []struct {
		name     string
		threadID int
		want     string
	}{
		{"topic", 2, "thread=2 inherit=kept"},
		{"no topic", 0, "thread= inherit=kept"},
		{"negative", -1, "thread= inherit=kept"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "env.txt")
			path := writeScript(t,
				"#!/bin/sh\necho \"thread=$TELEGRAM_THREAD_ID inherit=$TGSHD_TEST_INHERIT\" > "+out+"\n")
			s := New(zap.NewNop(), path)

			if err := s.Execute(context.Background(), "arg", tt.threadID); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(got)) != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
