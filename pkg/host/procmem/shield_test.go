//go:build linux

package procmem

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestShieldWritesOOMScoreAndLocksMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oom_score_adj")
	var gotFlags uintptr
	s := &Shield{
		oomPath: path,
		mlockall: func(flags uintptr) error {
			gotFlags = flags
			return nil
		},
	}
	if err := s.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "-1000" {
		t.Errorf("oom_score_adj = %q, want -1000", data)
	}
	want := uintptr(syscall.MCL_CURRENT | syscall.MCL_FUTURE | 4)
	if gotFlags != want {
		t.Errorf("mlockall flags = %d, want %d", gotFlags, want)
	}
}

func TestShieldReportsBothFailures(t *testing.T) {
	lockErr := errors.New("lock denied")
	s := &Shield{
		oomPath:  filepath.Join(t.TempDir(), "missing", "oom_score_adj"),
		mlockall: func(uintptr) error { return lockErr },
	}
	err := s.Apply()
	if err == nil {
		t.Fatal("Apply: want error")
	}
	if !errors.Is(err, lockErr) {
		t.Errorf("error %v does not wrap the mlockall failure", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error %v does not wrap the oom_score_adj failure", err)
	}
}

func TestShieldOOMFailureStillLocksMemory(t *testing.T) {
	called := false
	s := &Shield{
		oomPath:  filepath.Join(t.TempDir(), "missing", "oom_score_adj"),
		mlockall: func(uintptr) error { called = true; return nil },
	}
	if err := s.Apply(); err == nil {
		t.Fatal("Apply: want error")
	}
	if !called {
		t.Error("mlockall was not attempted after the oom_score_adj failure")
	}
}

func TestSyscallMlockallRejectsBadFlags(t *testing.T) {
	// An invalid flag set must fail with EINVAL without locking anything.
	if err := syscallMlockall(1 << 20); err == nil {
		t.Fatal("want EINVAL for invalid flags")
	}
}

func TestNewShieldDefaults(t *testing.T) {
	s := NewShield()
	if s.oomPath != "/proc/self/oom_score_adj" || s.mlockall == nil {
		t.Errorf("unexpected defaults: %+v", s)
	}
}
