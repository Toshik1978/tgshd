package command

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/zte"
)

type fakeZTE struct {
	lastBearer zte.Bearer
	called     bool
	err        error
}

func (f *fakeZTE) SetBearer(_ context.Context, pref zte.Bearer) error {
	f.called = true
	f.lastBearer = pref
	return f.err
}

func TestNetworkName(t *testing.T) {
	cmd := NewNetworkCommand(zap.NewNop(), &fakePublisher{}, &fakeZTE{}, "5g")
	if got := cmd.Name(); got != "5g" {
		t.Errorf("Name() = %q, want %q", got, "5g")
	}
}

func TestNetworkEnabled(t *testing.T) {
	cmd := NewNetworkCommand(zap.NewNop(), &fakePublisher{}, &fakeZTE{}, "5g")
	if !cmd.Enabled() {
		t.Error("Enabled() = false, want true")
	}
}

func TestNetworkHandleHappyPath(t *testing.T) {
	cases := []struct {
		cmd        string
		wantBearer zte.Bearer
	}{
		{"5g", zte.BearerWLAnd5G},
		{"4g", zte.BearerWCDMAAndLTE},
		{"3g", zte.BearerOnlyWCDMA},
		{"unknown", zte.BearerWCDMAAndLTE}, // default fallback
	}
	for _, c := range cases {
		t.Run(c.cmd, func(t *testing.T) {
			pub := &fakePublisher{}
			conn := &fakeZTE{}
			cmd := NewNetworkCommand(zap.NewNop(), pub, conn, c.cmd)

			if err := cmd.Handle(context.Background(), 21, "/"+c.cmd); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !conn.called || conn.lastBearer != c.wantBearer {
				t.Errorf("expected SetBearer(%q), got called=%v bearer=%q", c.wantBearer, conn.called, conn.lastBearer)
			}
			if pub.count != 1 || pub.lastRecipient != 21 {
				t.Errorf("expected one reply to sender 21, got count=%d recipient=%d", pub.count, pub.lastRecipient)
			}
			wantSubstr := strings.ToUpper(c.cmd)
			if !strings.Contains(pub.lastMsg, wantSubstr) {
				t.Errorf("expected reply to contain %q, got %q", wantSubstr, pub.lastMsg)
			}
		})
	}
}

func TestNetworkHandleSetBearerError(t *testing.T) {
	pub := &fakePublisher{}
	conn := &fakeZTE{err: errors.New("boom")}
	cmd := NewNetworkCommand(zap.NewNop(), pub, conn, "5g")

	err := cmd.Handle(context.Background(), 21, "/5g")
	if err == nil {
		t.Fatal("expected an error when SetBearer fails")
	}
	if pub.count != 0 {
		t.Error("publisher must not be called when SetBearer fails")
	}
}

func TestNetworkHandlePublishError(t *testing.T) {
	pub := &errPublisher{err: errors.New("publish failed")}
	conn := &fakeZTE{}
	cmd := NewNetworkCommand(zap.NewNop(), pub, conn, "5g")

	err := cmd.Handle(context.Background(), 21, "/5g")
	if err == nil {
		t.Fatal("expected an error when publish fails")
	}
}
