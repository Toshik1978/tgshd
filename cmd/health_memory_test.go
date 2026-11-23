package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// scriptedLister answers ContainerNames with each error of errs in turn (nil
// lists one container); the last one repeats.
type scriptedLister struct {
	errs []error
	n    int
}

func (l *scriptedLister) ContainerNames(context.Context) (map[string]string, error) {
	err := l.errs[min(l.n, len(l.errs)-1)]
	l.n++
	if err != nil {
		return nil, err
	}

	return map[string]string{"id1": "web"}, nil
}

// noSocketErr is the shape of the Docker client's error when its unix socket
// does not exist: the dial's ENOENT stays reachable through the wrapping.
func noSocketErr() error {
	dial := &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", syscall.ENOENT)}

	return fmt.Errorf("list containers: failed to connect to the docker API: %w", dial)
}

func TestNameRefresherLogLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		errs      []error
		wantWarns int
		wantDebug int
	}{
		{name: "no socket on a host without docker", errs: []error{noSocketErr()}, wantDebug: 1},
		{name: "daemon down", errs: []error{errors.New("docker down")}, wantWarns: 1},
		{name: "socket gone after a list", errs: []error{nil, noSocketErr()}, wantWarns: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			core, logs := observer.New(zapcore.DebugLevel)
			r := &nameRefresher{
				names:  newContainerNames(),
				lister: &scriptedLister{errs: tt.errs},
				log:    zap.New(core),
				who:    "Memory check",
			}
			for range 3 {
				r.refresh(t.Context())
			}

			msgs := logs.FilterMessageSnippet("cannot list containers")
			if n := msgs.FilterLevelExact(zapcore.WarnLevel).Len(); n != tt.wantWarns {
				t.Errorf("warnings = %d, want %d", n, tt.wantWarns)
			}
			if n := msgs.FilterLevelExact(zapcore.DebugLevel).Len(); n != tt.wantDebug {
				t.Errorf("debug lines = %d, want %d", n, tt.wantDebug)
			}
		})
	}
}
