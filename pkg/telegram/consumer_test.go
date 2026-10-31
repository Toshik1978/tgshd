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

type fakeHandler struct {
	calls int
	got   string
}

func (h *fakeHandler) Name() string  { return "ping" }
func (h *fakeHandler) Enabled() bool { return true }
func (h *fakeHandler) Handle(_ context.Context, _ route.Origin, cmd string) error {
	h.calls++
	h.got = cmd
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
		{"allowed private", &telebot.Chat{ID: 100, Type: telebot.ChatPrivate}, &telebot.User{ID: 5}, 1, false},
		{"allowed channel", &telebot.Chat{ID: 200, Type: telebot.ChatChannel}, nil, 1, false},
		{"allowed private channel", &telebot.Chat{ID: 200, Type: telebot.ChatChannelPrivate}, nil, 1, false},
		{
			"group with allowed sender",
			&telebot.Chat{ID: 100, Type: telebot.ChatSuperGroup},
			&telebot.User{ID: 200},
			1,
			false,
		},
		{
			"plain group with allowed sender",
			&telebot.Chat{ID: 100, Type: telebot.ChatGroup},
			&telebot.User{ID: 200},
			1,
			false,
		},
		{"group with stranger", &telebot.Chat{ID: 100, Type: telebot.ChatSuperGroup}, &telebot.User{ID: 999}, 0, true},
		{"stranger private", &telebot.Chat{ID: 999, Type: telebot.ChatPrivate}, &telebot.User{ID: 999}, 0, true},
		{"stranger group", &telebot.Chat{ID: -999, Type: telebot.ChatSuperGroup}, &telebot.User{ID: 100}, 0, true},
		{"unknown chat type", &telebot.Chat{ID: 100}, &telebot.User{ID: 100}, 0, true},
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

func TestHandleCommandsBotName(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		username  string
		wantCalls int
	}{
		{"plain", "/ping", "tgshd_test_bot", 1},
		{"own bot", "/ping@tgshd_test_bot", "tgshd_test_bot", 1},
		{"other bot", "/ping@other_bot", "tgshd_test_bot", 0},
		{"username unknown", "/ping@tgshd_test_bot", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &fakeHandler{}
			c, _ := newTestConsumer(h)
			if tt.username != "" {
				c.bot = &telebot.Bot{Me: &telebot.User{Username: tt.username}}
			}
			chat := &telebot.Chat{ID: 100, Type: telebot.ChatChannel}
			tctx := fakeContext{msg: &telebot.Message{Chat: chat, Sender: &telebot.User{ID: 5}, Text: tt.text}}
			if err := c.handleCommands(context.Background(), map[string]Handler{"/ping": h}, tctx); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if h.calls != tt.wantCalls {
				t.Fatalf("handler calls = %d, want %d", h.calls, tt.wantCalls)
			}
		})
	}
}

func TestHandleCommandStripsOwnBotName(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{"own suffix", "/ping@tgshd_test_bot x", "/ping x"},
		{"own suffix keeps spacing", "/ping@TGSHD_test_bot   x  y", "/ping   x  y"},
		{"own suffix no args", "/ping@tgshd_test_bot", "/ping"},
		{"plain", "/ping x", "/ping x"},
		{"other bot untouched", "/ping@other_bot x", "/ping@other_bot x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &fakeHandler{}
			c, _ := newTestConsumer(h)
			c.bot = &telebot.Bot{Me: &telebot.User{Username: "tgshd_test_bot"}}
			chat := &telebot.Chat{ID: 100, Type: telebot.ChatChannel}
			tctx := fakeContext{msg: &telebot.Message{Chat: chat, Sender: &telebot.User{ID: 5}, Text: tt.text}}
			if err := c.handleCommand(context.Background(), h, tctx); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if h.got != tt.want {
				t.Fatalf("handler got %q, want %q", h.got, tt.want)
			}
		})
	}
}
