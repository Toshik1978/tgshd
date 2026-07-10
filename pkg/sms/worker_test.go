package sms

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/zte"
)

// publishCall records a single invocation of fakePublisher.Publish.
type publishCall struct {
	recipientID int64
	msg         string
}

// fakePublisher is a hand-written test double for the Publisher interface.
type fakePublisher struct {
	calls []publishCall
	// errs, if set, provides the error to return for the call at the
	// matching index (0-based). Calls beyond len(errs) return nil.
	errs []error
}

func (p *fakePublisher) Publish(_ context.Context, recipientID int64, msg string) error {
	idx := len(p.calls)
	p.calls = append(p.calls, publishCall{recipientID: recipientID, msg: msg})
	if idx < len(p.errs) {
		return p.errs[idx]
	}

	return nil
}

// fakeZTE is a hand-written test double for the ZTE interface.
type fakeZTE struct {
	sms []zte.Sms
	err error
}

func (z *fakeZTE) ReadSms(_ bool) ([]zte.Sms, error) {
	return z.sms, z.err
}

func TestWorker_Name(t *testing.T) {
	w := NewWorker(zap.NewNop(), &fakePublisher{}, 42, &fakeZTE{})
	if got := w.Name(); got != "sms" {
		t.Errorf("Name() = %q, want %q", got, "sms")
	}
}

func TestWorker_Duration(t *testing.T) {
	w := NewWorker(zap.NewNop(), &fakePublisher{}, 42, &fakeZTE{})
	if got := w.Duration(); got != 15*time.Second {
		t.Errorf("Duration() = %v, want %v", got, 15*time.Second)
	}
}

func TestWorker_Do_PublishesEachSms(t *testing.T) {
	const chatID = int64(1234)

	sms := []zte.Sms{
		{
			Number:  "+15550001111",
			Content: "00480069", // "Hi"
			Date:    "2024,1,2,3,4,5",
		},
		{
			Number:  "+15552223333",
			Content: "00420079", // "By"
			Date:    "2024,6,7,8,9,10",
		},
	}
	pub := &fakePublisher{}
	conn := &fakeZTE{sms: sms}
	w := NewWorker(zap.NewNop(), pub, chatID, conn)

	err := w.Do(context.Background())
	if err != nil {
		t.Fatalf("Do() unexpected error: %v", err)
	}

	if len(pub.calls) != 2 {
		t.Fatalf("Publish called %d times, want 2", len(pub.calls))
	}

	for i, call := range pub.calls {
		if call.recipientID != chatID {
			t.Errorf("call[%d].recipientID = %d, want %d", i, call.recipientID, chatID)
		}
		if !strings.Contains(call.msg, sms[i].Number) {
			t.Errorf("call[%d].msg = %q, want it to contain sender number %q", i, call.msg, sms[i].Number)
		}
	}

	if !strings.Contains(pub.calls[0].msg, "Hi") {
		t.Errorf("call[0].msg = %q, want it to contain decoded content %q", pub.calls[0].msg, "Hi")
	}
	if !strings.Contains(pub.calls[1].msg, "By") {
		t.Errorf("call[1].msg = %q, want it to contain decoded content %q", pub.calls[1].msg, "By")
	}
}

func TestWorker_Do_ReadSmsError(t *testing.T) {
	readErr := errors.New("read sms failed")
	pub := &fakePublisher{}
	conn := &fakeZTE{err: readErr}
	w := NewWorker(zap.NewNop(), pub, 1, conn)

	err := w.Do(context.Background())
	if err == nil {
		t.Fatal("Do() error = nil, want non-nil")
	}
	if !errors.Is(err, readErr) {
		t.Errorf("Do() error = %v, want it to wrap %v", err, readErr)
	}
	if len(pub.calls) != 0 {
		t.Errorf("Publish called %d times, want 0", len(pub.calls))
	}
}

func TestWorker_Do_PublisherError(t *testing.T) {
	pubErr := errors.New("publish failed")
	sms := []zte.Sms{
		{
			Number:  "+15550001111",
			Content: "0048",
			Date:    "2024,1,2,3,4,5",
		},
	}
	pub := &fakePublisher{errs: []error{pubErr}}
	conn := &fakeZTE{sms: sms}
	w := NewWorker(zap.NewNop(), pub, 1, conn)

	err := w.Do(context.Background())
	if err == nil {
		t.Fatal("Do() error = nil, want non-nil")
	}
	if !errors.Is(err, pubErr) {
		t.Errorf("Do() error = %v, want it to wrap %v", err, pubErr)
	}
	if len(pub.calls) != 1 {
		t.Errorf("Publish called %d times, want 1", len(pub.calls))
	}
}

func TestWorker_Do_ReadAndPublisherErrorsAreJoined(t *testing.T) {
	// Even when ReadSms returns an error, any sms already returned alongside
	// it should still be attempted, and both errors should surface via
	// errors.Join.
	readErr := errors.New("partial read failure")
	pubErr := errors.New("publish failed")
	sms := []zte.Sms{
		{
			Number:  "+15550001111",
			Content: "0048",
			Date:    "2024,1,2,3,4,5",
		},
	}
	pub := &fakePublisher{errs: []error{pubErr}}
	conn := &fakeZTE{sms: sms, err: readErr}
	w := NewWorker(zap.NewNop(), pub, 1, conn)

	err := w.Do(context.Background())
	if err == nil {
		t.Fatal("Do() error = nil, want non-nil")
	}
	if !errors.Is(err, readErr) {
		t.Errorf("Do() error = %v, want it to wrap %v", err, readErr)
	}
	if !errors.Is(err, pubErr) {
		t.Errorf("Do() error = %v, want it to wrap %v", err, pubErr)
	}
}

func TestWorker_Do_EmptySmsList(t *testing.T) {
	pub := &fakePublisher{}
	conn := &fakeZTE{sms: []zte.Sms{}}
	w := NewWorker(zap.NewNop(), pub, 1, conn)

	err := w.Do(context.Background())
	if err != nil {
		t.Fatalf("Do() unexpected error: %v", err)
	}
	if len(pub.calls) != 0 {
		t.Errorf("Publish called %d times, want 0", len(pub.calls))
	}
}
