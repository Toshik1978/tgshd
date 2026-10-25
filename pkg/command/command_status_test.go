package command

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/health/report"
	"github.com/Toshik1978/tgshd/pkg/health/runner"
)

type fakeHealth struct {
	snap    runner.Snapshot
	cleared []string
	ok      bool
}

func (f *fakeHealth) Snapshot() runner.Snapshot { return f.snap }

func (f *fakeHealth) Clear(key string) bool {
	f.cleared = append(f.cleared, key)
	return f.ok
}

func TestStatusCommand(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	pub := &fakePublisher{}
	src := &fakeHealth{snap: runner.Snapshot{Open: []runner.OpenCondition{{Key: "disk/a", Text: "hot", Since: at}}}}
	cmd := NewStatusCommand(zap.NewNop(), pub, src, nil)

	if cmd.Name() != "status" || !cmd.Enabled() {
		t.Fatalf("Name=%q Enabled=%v", cmd.Name(), cmd.Enabled())
	}
	if err := cmd.Handle(context.Background(), 5, "/status"); err != nil {
		t.Fatal(err)
	}
	if pub.lastRecipient != 5 || !strings.Contains(pub.lastMsg, "disk/a: hot (since 2026-01-02 03:04)") {
		t.Errorf("recipient=%d msg=%q", pub.lastRecipient, pub.lastMsg)
	}
}

// handleKey marks the context Handle was given.
type handleKey struct{}

// fakeMemory is the memory block source of /status.
type fakeMemory struct {
	mem *report.Memory
	ctx context.Context //nolint:containedctx // records the context Handle passed
}

func (f *fakeMemory) MemoryStatus(ctx context.Context) *report.Memory {
	f.ctx = ctx
	return f.mem
}

func TestStatusCommandShowsTheMemoryBlock(t *testing.T) {
	pub := &fakePublisher{}
	mem := &fakeMemory{mem: &report.Memory{Unreadable: true, Guard: &report.Guard{Mode: "off"}}}
	cmd := NewStatusCommand(zap.NewNop(), pub, &fakeHealth{snap: runner.Snapshot{Checks: 1}}, mem)

	ctx := context.WithValue(context.Background(), handleKey{}, "handle")
	if err := cmd.Handle(ctx, 5, "/status"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(pub.lastMsg, "\nMemory: unreadable\nGuard: off") || mem.ctx != ctx {
		t.Errorf("msg = %q (memory read with the handler's context: %v)", pub.lastMsg, mem.ctx == ctx)
	}
}

func TestStatusCommandPublishError(t *testing.T) {
	pub := &errPublisher{err: errors.New("boom")}
	cmd := NewStatusCommand(zap.NewNop(), pub, &fakeHealth{}, nil)
	if err := cmd.Handle(context.Background(), 1, "/status"); !errors.Is(err, pub.err) {
		t.Errorf("err = %v", err)
	}
}

func TestClearCommand(t *testing.T) {
	open := runner.Snapshot{Open: []runner.OpenCondition{{Key: "a/1"}, {Key: "b/<2>"}}}
	cases := []struct {
		name        string
		cmd         string
		ok          bool
		wantCleared []string
		wantMsg     []string
	}{
		{"cleared", "/clear a/1", true, []string{"a/1"}, []string{"Cleared a/1 (no RESOLVED sent)."}},
		{
			"cleared escaped",
			"/clear b/<2>",
			true,
			[]string{"b/<2>"},
			[]string{"Cleared b/&lt;2&gt; (no RESOLVED sent)."},
		},
		{"unknown", "/clear nope", false, []string{"nope"}, []string{"Unknown condition nope", "a/1", "b/&lt;2&gt;"}},
		{"no arg", "/clear", false, nil, []string{"Usage: /clear", "a/1", "b/&lt;2&gt;"}},
		{"escaped key", "/clear <x>", false, []string{"<x>"}, []string{"Unknown condition &lt;x&gt;"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pub := &fakePublisher{}
			h := &fakeHealth{snap: open, ok: c.ok}
			cmd := NewClearCommand(zap.NewNop(), pub, h)
			if cmd.Name() != "clear" || !cmd.Enabled() {
				t.Fatalf("Name=%q Enabled=%v", cmd.Name(), cmd.Enabled())
			}
			if err := cmd.Handle(context.Background(), 9, c.cmd); err != nil {
				t.Fatal(err)
			}
			if strings.Join(h.cleared, ",") != strings.Join(c.wantCleared, ",") {
				t.Errorf("cleared = %v, want %v", h.cleared, c.wantCleared)
			}
			for _, w := range c.wantMsg {
				if !strings.Contains(pub.lastMsg, w) {
					t.Errorf("missing %q in %q", w, pub.lastMsg)
				}
			}
		})
	}
}

func TestClearCommandPublishError(t *testing.T) {
	pub := &errPublisher{err: errors.New("boom")}
	cmd := NewClearCommand(zap.NewNop(), pub, &fakeHealth{})
	if err := cmd.Handle(context.Background(), 1, "/clear"); !errors.Is(err, pub.err) {
		t.Errorf("err = %v", err)
	}
}
