package systemctl

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type fakeRunner struct {
	calls  [][]string
	stderr string
	err    error
}

func (f *fakeRunner) run(_ context.Context, argv []string) (string, error) {
	f.calls = append(f.calls, argv)

	return f.stderr, f.err
}

func TestRestartAndStopRunSystemctl(t *testing.T) {
	tests := []struct {
		verb string
		call func(*Client, context.Context, string) error
	}{
		{"restart", (*Client).Restart},
		{"stop", (*Client).Stop},
	}
	for _, tt := range tests {
		t.Run(tt.verb, func(t *testing.T) {
			f := &fakeRunner{}
			c := &Client{binary: "systemctl", run: f.run}
			if err := tt.call(c, t.Context(), "web-app@1.service"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			want := []string{"systemctl", "--no-ask-password", tt.verb, "--", "web-app@1.service"}
			if len(f.calls) != 1 || !reflect.DeepEqual(f.calls[0], want) {
				t.Fatalf("calls = %v, want [%v]", f.calls, want)
			}
		})
	}
}

func TestInvalidUnitNeverExecuted(t *testing.T) {
	for _, unit := range []string{
		"", "--foo", "--foo.service", "web-app", "web app.service", "a/b.service",
		".service", "web.service\n", "web.service;rm", "web.timer", "-h.service ",
	} {
		f := &fakeRunner{}
		c := &Client{binary: "systemctl", run: f.run}
		for name, fn := range map[string]func(context.Context, string) error{"restart": c.Restart, "stop": c.Stop} {
			err := fn(t.Context(), unit)
			if !errors.Is(err, ErrInvalidUnit) {
				t.Errorf("%s(%q) error = %v, want ErrInvalidUnit", name, unit, err)
			}
		}
		if len(f.calls) != 0 {
			t.Errorf("unit %q executed: %v", unit, f.calls)
		}
	}
}

func TestFailureIncludesBoundedStderr(t *testing.T) {
	cause := errors.New("exit status 5")
	f := &fakeRunner{stderr: "  Unit web-app.service not loaded.\n", err: cause}
	c := &Client{binary: "systemctl", run: f.run}
	err := c.Restart(t.Context(), "web-app.service")
	if !errors.Is(err, cause) {
		t.Fatalf("error %v does not wrap the cause", err)
	}
	if !strings.Contains(err.Error(), "Unit web-app.service not loaded.") || strings.HasSuffix(err.Error(), "\n") {
		t.Errorf("error = %q, want trimmed stderr", err)
	}

	f = &fakeRunner{stderr: strings.Repeat("x", 3*maxStderr), err: cause}
	c = &Client{binary: "systemctl", run: f.run}
	if err := c.Stop(t.Context(), "web-app.service"); len(err.Error()) > maxStderr+100 {
		t.Errorf("error length %d not bounded", len(err.Error()))
	}
}

func TestExecRunnerCancelAndMissingBinary(t *testing.T) {
	c := New("/nonexistent/systemctl-binary")
	if err := c.Restart(t.Context(), "web-app.service"); err == nil {
		t.Fatal("expected error for missing binary")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c = New("sleep")
	// "sleep --no-ask-password ..." fails anyway; a cancelled context must also be an error.
	if err := c.Stop(ctx, "web-app.service"); err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestExecRunnerCapturesStderr(t *testing.T) {
	c := New("ls") // ls rejects the unknown option and writes to stderr
	err := c.Restart(t.Context(), "web-app.service")
	if err == nil || !strings.Contains(err.Error(), "no-ask-password") {
		t.Fatalf("error = %v, want stderr mentioning the option", err)
	}
}
