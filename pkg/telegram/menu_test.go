package telegram

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gopkg.in/telebot.v4"

	"github.com/Toshik1978/tgshd/pkg/telegram/route"
)

type setterCall struct {
	method string
	opts   []any
}

type fakeSetter struct {
	calls     []setterCall
	deleteErr error
	setErrs   map[int]error // keyed by zero-based set call index
	afterSet  func(n int)   // called after each set with its zero-based index
	sets      int
}

func (f *fakeSetter) SetCommands(opts ...any) error {
	f.calls = append(f.calls, setterCall{"set", opts})
	n := f.sets
	f.sets++
	if f.afterSet != nil {
		f.afterSet(n)
	}

	return f.setErrs[n]
}

func (f *fakeSetter) DeleteCommands(opts ...any) error {
	f.calls = append(f.calls, setterCall{"delete", opts})
	return f.deleteErr
}

func newMenuConsumer(s commandSetter) (*consumer, *observer.ObservedLogs) {
	core, logs := observer.New(zap.DebugLevel)

	return &consumer{logger: zap.New(core), setter: s, access: route.NewAccessList(0, nil)}, logs
}

const (
	menuUserA  int64 = 100000000
	menuUserB  int64 = 200000000
	menuGroup  int64 = -1000000000000
	setTypeSet       = "set"
)

func testMenu() []route.BotCommand {
	return []route.BotCommand{
		{Name: "ping", Description: "Ping a host"},
		{Name: "status", Description: "Show health status"},
	}
}

func testTelebotMenu() []telebot.Command {
	return []telebot.Command{
		{Text: "ping", Description: "Ping a host"},
		{Text: "status", Description: "Show health status"},
	}
}

func newScopedConsumer(s commandSetter, menu []route.BotCommand) (*consumer, *observer.ObservedLogs) {
	c, logs := newMenuConsumer(s)
	c.access = route.NewAccessList(menuUserA, []int64{menuUserB, menuGroup})
	c.menu = menu

	return c, logs
}

func setCalls(s *fakeSetter) []setterCall {
	var out []setterCall
	for _, call := range s.calls {
		if call.method == setTypeSet {
			out = append(out, call)
		}
	}

	return out
}

func TestPublishMenuSetsEveryScope(t *testing.T) {
	s := &fakeSetter{}
	c, _ := newScopedConsumer(s, testMenu())

	c.publishMenu(context.Background())

	wantScopes := []telebot.CommandScope{
		{Type: telebot.CommandScopeChat, ChatID: menuUserA},
		{Type: telebot.CommandScopeChat, ChatID: menuUserB},
		{Type: telebot.CommandScopeChatMember, ChatID: menuGroup, UserID: menuUserA},
		{Type: telebot.CommandScopeChatMember, ChatID: menuGroup, UserID: menuUserB},
	}
	wantMenu := testTelebotMenu()
	if len(s.calls) != 1+len(wantScopes) || s.calls[0].method != "delete" {
		t.Fatalf("want delete then %d sets, got %+v", len(wantScopes), s.calls)
	}
	for i, want := range wantScopes {
		call := s.calls[i+1]
		if call.method != setTypeSet || len(call.opts) != 2 {
			t.Fatalf("call %d: want set with two opts, got %+v", i, call)
		}
		cmds, ok := call.opts[0].([]telebot.Command)
		if !ok || len(cmds) != len(wantMenu) || cmds[0] != wantMenu[0] || cmds[1] != wantMenu[1] {
			t.Fatalf("call %d: want menu %+v, got %+v", i, wantMenu, call.opts[0])
		}
		if call.opts[1] != want {
			t.Fatalf("call %d: want scope %+v, got %+v", i, want, call.opts[1])
		}
	}
}

func TestPublishMenuLogsPublishedCount(t *testing.T) {
	s := &fakeSetter{}
	c, logs := newScopedConsumer(s, testMenu())

	c.publishMenu(context.Background())

	infos := logs.FilterLevelExact(zap.InfoLevel).All()
	if len(infos) != 1 {
		t.Fatalf("want one info log, got %d", len(infos))
	}
	if got := infos[0].ContextMap()["scopes"]; got != int64(4) {
		t.Fatalf("want scopes 4, got %v", got)
	}
}

func TestPublishMenuFailedScopeLoggedAndNextProceeds(t *testing.T) {
	s := &fakeSetter{setErrs: map[int]error{2: errors.New("rejected")}}
	c, logs := newScopedConsumer(s, testMenu())

	c.publishMenu(context.Background())

	if got := len(setCalls(s)); got != 4 {
		t.Fatalf("want 4 set calls despite failure, got %d", got)
	}
	warns := logs.FilterLevelExact(zap.WarnLevel).All()
	if len(warns) != 1 {
		t.Fatalf("want one warn, got %d", len(warns))
	}
	fields := warns[0].ContextMap()
	if fields["scope"] != telebot.CommandScopeChatMember || fields["chat_id"] != menuGroup ||
		fields["user_id"] != menuUserA || fields["error"] != "rejected" {
		t.Fatalf("unexpected warn fields %v", fields)
	}
	infos := logs.FilterLevelExact(zap.InfoLevel).All()
	if len(infos) != 1 || infos[0].ContextMap()["scopes"] != int64(3) {
		t.Fatalf("want one info with scopes 3, got %+v", infos)
	}
}

func TestPublishMenuNoSetCalls(t *testing.T) {
	tests := []struct {
		name string
		c    func(s commandSetter) *consumer
	}{
		{"no scopes", func(s commandSetter) *consumer {
			c, _ := newMenuConsumer(s)
			c.menu = testMenu()

			return c
		}},
		{"empty menu", func(s commandSetter) *consumer {
			c, _ := newScopedConsumer(s, nil)

			return c
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &fakeSetter{}

			tt.c(s).publishMenu(context.Background())

			if len(s.calls) != 1 || s.calls[0].method != "delete" {
				t.Fatalf("want only the default delete, got %+v", s.calls)
			}
		})
	}
}

func TestPublishMenuCancelStopsFurtherCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &fakeSetter{afterSet: func(n int) {
		if n == 0 {
			cancel()
		}
	}}
	c, _ := newScopedConsumer(s, testMenu())

	c.publishMenu(ctx)

	if got := len(setCalls(s)); got != 1 {
		t.Fatalf("want 1 set call before cancel, got %d", got)
	}
}

func TestPublishMenuDeletesDefaultScope(t *testing.T) {
	s := &fakeSetter{}
	c, _ := newMenuConsumer(s)

	c.publishMenu(context.Background())

	if len(s.calls) != 1 || s.calls[0].method != "delete" {
		t.Fatalf("want one delete call, got %+v", s.calls)
	}
	want := telebot.CommandScope{Type: telebot.CommandScopeDefault}
	if len(s.calls[0].opts) != 1 || s.calls[0].opts[0] != want {
		t.Fatalf("want scope %+v, got %+v", want, s.calls[0].opts)
	}
}

func TestPublishMenuCancelledContextCallsNothing(t *testing.T) {
	s := &fakeSetter{}
	c, _ := newMenuConsumer(s)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c.publishMenu(ctx)

	if len(s.calls) != 0 {
		t.Fatalf("want no calls, got %+v", s.calls)
	}
}

func TestPublishMenuLogsDeleteFailure(t *testing.T) {
	s := &fakeSetter{deleteErr: errors.New("boom")}
	c, logs := newMenuConsumer(s)

	c.publishMenu(context.Background())

	warns := logs.FilterLevelExact(zap.WarnLevel).All()
	if len(warns) != 1 {
		t.Fatalf("want one warn, got %d", len(warns))
	}
	if got := warns[0].ContextMap()["error"]; got != "boom" {
		t.Fatalf("want error field boom, got %v", got)
	}
}
