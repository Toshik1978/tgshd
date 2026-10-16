package telegram

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gopkg.in/telebot.v4"
)

func TestAccessListAllowed(t *testing.T) {
	tests := []struct {
		name   string
		chatID int64
		extra  []int64
		query  int64
		want   bool
	}{
		{"configured chat", 100, nil, 100, true},
		{"extra id", 100, []int64{200, -300}, -300, true},
		{"stranger", 100, []int64{200}, 999, false},
		{"unset chat does not allow zero", 0, nil, 0, false},
		{"unset chat with extras", 0, []int64{200}, 200, true},
		{"empty list denies all", 0, nil, 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NewAccessList(tt.chatID, tt.extra).Allowed(tt.query); got != tt.want {
				t.Fatalf("Allowed(%d) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestAccessListEmpty(t *testing.T) {
	if !NewAccessList(0, nil).Empty() {
		t.Fatal("list with no IDs must be empty")
	}

	if NewAccessList(1, nil).Empty() {
		t.Fatal("list with an ID must not be empty")
	}
}

func TestNewConsumerWarnsOnEmptyAccessList(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	NewConsumer(zap.New(core), nil, NewAccessList(0, nil), nil)

	if got := logs.FilterLevelExact(zap.WarnLevel).Len(); got != 1 {
		t.Fatalf("want one warn, got %d", got)
	}
}

type fakeContext struct {
	telebot.Context

	msg *telebot.Message
}

func (f fakeContext) Message() *telebot.Message { return f.msg }
func (f fakeContext) Chat() *telebot.Chat       { return f.msg.Chat }
func (f fakeContext) Text() string              { return f.msg.Text }

type fakeHandler struct{ calls int }

func (h *fakeHandler) Name() string  { return "ping" }
func (h *fakeHandler) Enabled() bool { return true }
func (h *fakeHandler) Handle(context.Context, int64, string) error {
	h.calls++
	return nil
}

func newTestConsumer(h Handler) (*consumer, *observer.ObservedLogs) {
	core, logs := observer.New(zap.DebugLevel)

	return &consumer{
		logger:   zap.New(core),
		access:   NewAccessList(100, []int64{200}),
		handlers: []Handler{h},
	}, logs
}

func TestConsumerAccessFilter(t *testing.T) {
	tests := []struct {
		name      string
		chat      *telebot.Chat
		sender    *telebot.User
		wantCalls int
		wantWarn  bool
	}{
		{"allowed chat", &telebot.Chat{ID: 100}, &telebot.User{ID: 5}, 1, false},
		{"allowed extra", &telebot.Chat{ID: 200}, nil, 1, false},
		{"stranger private", &telebot.Chat{ID: 999}, &telebot.User{ID: 999}, 0, true},
		{"stranger group", &telebot.Chat{ID: -999}, &telebot.User{ID: 100}, 0, true},
		{"no chat", nil, &telebot.User{ID: 100}, 0, true},
	}
	for _, tt := range tests {
		for _, path := range []string{"command", "channel"} {
			t.Run(tt.name+"/"+path, func(t *testing.T) {
				h := &fakeHandler{}
				c, logs := newTestConsumer(h)
				tctx := fakeContext{msg: &telebot.Message{Chat: tt.chat, Sender: tt.sender, Text: "/ping"}}
				var err error
				if path == "command" {
					err = c.handleCommand(context.Background(), h, tctx)
				} else {
					err = c.handleCommands(context.Background(), map[string]Handler{"/ping": h}, tctx)
				}
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if h.calls != tt.wantCalls {
					t.Fatalf("handler calls = %d, want %d", h.calls, tt.wantCalls)
				}
				warns := logs.FilterLevelExact(zap.WarnLevel).Len()
				if tt.wantWarn != (warns == 1) || warns > 1 {
					t.Fatalf("warn logs = %d, wantWarn %v", warns, tt.wantWarn)
				}
			})
		}
	}
}
