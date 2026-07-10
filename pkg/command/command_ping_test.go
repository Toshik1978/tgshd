package command

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"
)

type fakePinger struct {
	result map[string]bool
	err    error
}

func (f *fakePinger) Ping(_ context.Context, _ []string) (map[string]bool, error) {
	return f.result, f.err
}

func TestPingName(t *testing.T) {
	cmd := NewPingCommand(zap.NewNop(), &fakePublisher{}, &fakePinger{}, []string{"host1"})
	if got := cmd.Name(); got != "ping" {
		t.Errorf("Name() = %q, want %q", got, "ping")
	}
}

func TestPingEnabled(t *testing.T) {
	cases := []struct {
		name  string
		hosts []string
		want  bool
	}{
		{"with hosts", []string{"host1", "host2"}, true},
		{"empty hosts", []string{}, false},
		{"nil hosts", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := NewPingCommand(zap.NewNop(), &fakePublisher{}, &fakePinger{}, c.hosts)
			if got := cmd.Enabled(); got != c.want {
				t.Errorf("Enabled() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestPingHandleHappyPath(t *testing.T) {
	pub := &fakePublisher{}
	pinger := &fakePinger{result: map[string]bool{"host1": true, "host2": false}}
	cmd := NewPingCommand(zap.NewNop(), pub, pinger, []string{"host1", "host2"})

	if err := cmd.Handle(context.Background(), 11, "/ping"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pub.count != 1 || pub.lastRecipient != 11 {
		t.Errorf("expected one reply to sender 11, got count=%d recipient=%d", pub.count, pub.lastRecipient)
	}
	if !strings.Contains(pub.lastMsg, "host1: <b>OK</b>") {
		t.Errorf("expected reply to report host1 OK, got %q", pub.lastMsg)
	}
	if !strings.Contains(pub.lastMsg, "host2: <b>FAIL</b>") {
		t.Errorf("expected reply to report host2 FAIL, got %q", pub.lastMsg)
	}
}

func TestPingHandleMissingHostTreatedAsFail(t *testing.T) {
	pub := &fakePublisher{}
	// host2 missing from result map entirely.
	pinger := &fakePinger{result: map[string]bool{"host1": true}}
	cmd := NewPingCommand(zap.NewNop(), pub, pinger, []string{"host1", "host2"})

	if err := cmd.Handle(context.Background(), 11, "/ping"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(pub.lastMsg, "host2: <b>FAIL</b>") {
		t.Errorf("expected missing host to be reported as FAIL, got %q", pub.lastMsg)
	}
}

func TestPingHandlePingError(t *testing.T) {
	pub := &fakePublisher{}
	pinger := &fakePinger{err: errors.New("boom")}
	cmd := NewPingCommand(zap.NewNop(), pub, pinger, []string{"host1"})

	err := cmd.Handle(context.Background(), 11, "/ping")
	if err == nil {
		t.Fatal("expected an error when Ping fails")
	}
	if pub.count != 0 {
		t.Error("publisher must not be called when Ping fails")
	}
}

func TestPingHandlePublishError(t *testing.T) {
	pub := &errPublisher{err: errors.New("publish failed")}
	pinger := &fakePinger{result: map[string]bool{"host1": true}}
	cmd := NewPingCommand(zap.NewNop(), pub, pinger, []string{"host1"})

	err := cmd.Handle(context.Background(), 11, "/ping")
	if err == nil {
		t.Fatal("expected an error when publish fails")
	}
}
