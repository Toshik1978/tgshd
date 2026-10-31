package telegram

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gopkg.in/telebot.v4"

	"github.com/Toshik1978/tgshd/pkg/telegram/route"
)

func TestNewConsumerWarnsOnEmptyAccessList(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	NewConsumer(zap.New(core), nil, route.NewAccessList(0, nil), nil)

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
func (h *fakeHandler) Handle(context.Context, route.Origin, string) error {
	h.calls++
	return nil
}

func newTestConsumer(h Handler) (*consumer, *observer.ObservedLogs) {
	core, logs := observer.New(zap.DebugLevel)

	return &consumer{
		logger:   zap.New(core),
		access:   route.NewAccessList(100, []int64{200}),
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

func TestOriginOf(t *testing.T) {
	chat := &telebot.Chat{ID: -1000000000000}
	tests := []struct {
		name string
		msg  *telebot.Message
		want route.Origin
	}{
		{
			"topic message",
			&telebot.Message{Chat: chat, Sender: &telebot.User{ID: 100}, ThreadID: 3, TopicMessage: true},
			route.Origin{ChatID: -1000000000000, ThreadID: 3, SenderID: 100},
		},
		{
			"reply in General carries a thread id but is not a topic message",
			&telebot.Message{Chat: chat, Sender: &telebot.User{ID: 100}, ThreadID: 2},
			route.Origin{ChatID: -1000000000000, SenderID: 100},
		},
		{
			"channel post without sender",
			&telebot.Message{Chat: chat},
			route.Origin{ChatID: -1000000000000},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := originOf(tt.msg); got != tt.want {
				t.Fatalf("originOf() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
