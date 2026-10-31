package command

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/telegram/route"
)

type fakeSender struct {
	called bool
	phone  string
	msg    string
	err    error
}

func (f *fakeSender) Publish(_ context.Context, phone, msg string) error {
	f.called = true
	f.phone = phone
	f.msg = msg
	return f.err
}

type fakePublisher struct {
	lastDest route.Destination
	lastMsg  string
	count    int
}

func (f *fakePublisher) Publish(_ context.Context, d route.Destination, msg string) error {
	f.count++
	f.lastDest = d
	f.lastMsg = msg
	return nil
}

func TestSmsParseArgs(t *testing.T) {
	cases := []struct {
		in        string
		wantPhone string
		wantBody  string
	}{
		{"/sms +1234567890 hello world", "+1234567890", "hello world"},
		{"/sms +1234567890 single", "+1234567890", "single"},
		{"/sms", "", ""},
		{"/sms +1234567890", "", ""},
	}
	for _, c := range cases {
		phone, body := parseSmsArgs(c.in)
		if phone != c.wantPhone || body != c.wantBody {
			t.Errorf("parseSmsArgs(%q) = (%q, %q), want (%q, %q)", c.in, phone, body, c.wantPhone, c.wantBody)
		}
	}
}

func TestSmsUnauthorizedSenderIgnored(t *testing.T) {
	sender := &fakeSender{}
	pub := &fakePublisher{}
	cmd := NewSmsCommand(zap.NewNop(), pub, sender, 100)

	if err := cmd.Handle(context.Background(), testOrigin(999), "/sms +123 hi"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sender.called {
		t.Error("sender must NOT be called for an unauthorized sender")
	}
	if pub.count != 0 {
		t.Error("no reply must be sent to an unauthorized sender")
	}
}

func TestSmsComparesChatNotSender(t *testing.T) {
	sender := &fakeSender{}
	pub := &fakePublisher{}
	cmd := NewSmsCommand(zap.NewNop(), pub, sender, 100)

	// The sender id equals the configured chat, but the command came from another chat.
	origin := route.Origin{ChatID: 999, ThreadID: 3, SenderID: 100}
	if err := cmd.Handle(context.Background(), origin, "/sms +123 hi"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sender.called || pub.count != 0 {
		t.Errorf("command from another chat must be ignored, sent=%v replies=%d", sender.called, pub.count)
	}
}

func TestSmsHappyPath(t *testing.T) {
	sender := &fakeSender{}
	pub := &fakePublisher{}
	cmd := NewSmsCommand(zap.NewNop(), pub, sender, 100)

	if err := cmd.Handle(context.Background(), testOrigin(100), "/sms +123 hello there"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !sender.called || sender.phone != "+123" || sender.msg != "hello there" {
		t.Errorf("sender got (%v, %q, %q)", sender.called, sender.phone, sender.msg)
	}
	if pub.count != 1 || pub.lastDest != (route.Destination{ChatID: 100, ThreadID: 3, Silent: true}) {
		t.Errorf("expected one confirmation to sender 100, got count=%d dest=%+v", pub.count, pub.lastDest)
	}
}

func TestSmsUsageOnBadArgs(t *testing.T) {
	sender := &fakeSender{}
	pub := &fakePublisher{}
	cmd := NewSmsCommand(zap.NewNop(), pub, sender, 100)

	if err := cmd.Handle(context.Background(), testOrigin(100), "/sms"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sender.called {
		t.Error("sender must not be called on bad args")
	}
	if pub.count != 1 {
		t.Error("expected a usage reply")
	}
	if !strings.Contains(pub.lastMsg, "&lt;") || strings.Contains(pub.lastMsg, "<phone") {
		t.Errorf("usage reply must be HTML-escaped, got %q", pub.lastMsg)
	}
}

func TestSmsSendFailureReported(t *testing.T) {
	sender := &fakeSender{err: errors.New("connection <failed> & down")}
	pub := &fakePublisher{}
	cmd := NewSmsCommand(zap.NewNop(), pub, sender, 100)

	err := cmd.Handle(context.Background(), testOrigin(100), "/sms +123 hi")
	if err == nil {
		t.Fatal("expected an error when the sender fails")
	}
	if pub.count != 1 {
		t.Error("expected an error reply to the sender")
	}
	if strings.Contains(pub.lastMsg, "<failed>") {
		t.Errorf("failure reply must be HTML-escaped, got %q", pub.lastMsg)
	}
	if !strings.Contains(pub.lastMsg, "&lt;failed&gt;") {
		t.Errorf("failure reply must contain escaped error, got %q", pub.lastMsg)
	}
}
