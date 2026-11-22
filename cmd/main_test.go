package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.uber.org/fx"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/Toshik1978/tgshd/cmd/app"
	"github.com/Toshik1978/tgshd/pkg/telegram"
	"github.com/Toshik1978/tgshd/pkg/telegram/route"
	"github.com/Toshik1978/tgshd/pkg/zte"
)

func TestNewTopics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		cfg          config
		want         route.Topics
		wantWarnings int
	}{
		{"all unset", config{}, route.Topics{}, 0},
		{
			"all set",
			config{TopicAlerts: "2", TopicSMS: "3", TopicSystem: "4"},
			route.Topics{Alerts: 2, SMS: 3, System: 4},
			0,
		},
		{"one missing", config{TopicAlerts: "2", TopicSMS: "3"}, route.Topics{Alerts: 2, SMS: 3}, 1},
		{"invalid and missing", config{TopicAlerts: "abc"}, route.Topics{}, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			core, logs := observer.New(zap.WarnLevel)
			got := newTopics(&tt.cfg, zap.New(core))
			if got != tt.want {
				t.Fatalf("newTopics() = %+v, want %+v", got, tt.want)
			}
			if logs.Len() != tt.wantWarnings {
				t.Fatalf("logged %d warnings, want %d: %v", logs.Len(), tt.wantWarnings, logs.All())
			}
		})
	}
}

type stubZTE struct{}

func (stubZTE) ReadSms(context.Context, bool) ([]zte.Sms, error) {
	return []zte.Sms{{Number: "+15550001111", Content: "00480069", Date: "2024,1,2,3,4,5"}}, nil
}

func TestNewModemSMSWorkerGoesToSMSTopic(t *testing.T) {
	t.Parallel()

	const chat = int64(-1000000000000)
	pub := &fakePublisher{}
	w := newModemSMSWorker(
		zap.NewNop(),
		pub,
		&config{ChatID: chat},
		route.Topics{Alerts: 2, SMS: 3, System: 4},
		stubZTE{},
	)
	if err := w.Do(t.Context()); err != nil {
		t.Fatal(err)
	}
	if want := (route.Destination{ChatID: chat, ThreadID: 3}); len(pub.to) != 1 || pub.to[0] != want {
		t.Fatalf("destinations = %+v, want [%+v] (sms topic, loud)", pub.to, want)
	}
}

// TestInvalidTopicWarnsWithoutOptionalFeatures: the TELEGRAM_TOPIC_* settings
// are parsed and warned about at every startup, even when no optional feature
// that consumes route.Topics is enabled (FR-004).
func TestInvalidTopicWarnsWithoutOptionalFeatures(t *testing.T) {
	t.Setenv("TELEGRAM_TOKEN", "123:placeholder")
	t.Setenv("TELEGRAM_TOPIC_ALERTS", "abc")
	t.Setenv("TELEGRAM_TOPIC_SMS", "3")
	t.Setenv("TELEGRAM_TOPIC_SYSTEM", "4")

	core, logs := observer.New(zap.WarnLevel)
	a := fx.New(append(appOptions(func(string) string { return "" }), fx.NopLogger,
		fx.Decorate(func(*zap.Logger) *zap.Logger { return zap.New(core) }))...)
	if err := a.Err(); err != nil {
		t.Fatalf("startup failed on a bad topic setting: %v", err)
	}
	var found int
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "TELEGRAM_TOPIC_ALERTS") {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("got %d TELEGRAM_TOPIC_ALERTS warnings, want 1: %v", found, logs.All())
	}
}

// TestTelegramPollSelectsConsumer: BOT_TELEGRAM_POLL=false wires the send-only
// consumer, which takes no handlers, so the handler group is never built.
func TestTelegramPollSelectsConsumer(t *testing.T) {
	tests := []struct {
		name     string
		poll     string
		wantSend bool
	}{
		{"false", "false", true},
		{"unset", "", false},
		{"true", "true", false},
		{"unparsable keeps polling", "nope", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TELEGRAM_TOKEN", "123:placeholder")
			getenv := func(k string) string {
				if k == "BOT_TELEGRAM_POLL" {
					return tt.poll
				}
				return ""
			}
			if err := fx.ValidateApp(appOptions(getenv)...); err != nil {
				t.Fatalf("graph invalid: %v", err)
			}

			var probeBuilt bool
			probe := fx.Provide(fx.Annotate(
				func() telegram.Handler { probeBuilt = true; return nil },
				fx.ResultTags(`group:"command_handler"`),
			))
			var consumer app.TelegramConsumer
			a := fx.New(append(appOptions(getenv), fx.NopLogger, probe, fx.Populate(&consumer))...)
			if err := a.Err(); err != nil {
				t.Fatalf("startup failed: %v", err)
			}
			isSend := strings.Contains(fmt.Sprintf("%T", consumer), "sendOnlyConsumer")
			if isSend != tt.wantSend {
				t.Fatalf("consumer = %T, send-only want %v", consumer, tt.wantSend)
			}
			if tt.wantSend && probeBuilt {
				t.Fatal("handler group was constructed in send-only mode")
			}
			if !tt.wantSend && !probeBuilt {
				t.Fatal("polling consumer did not take the handler group")
			}
		})
	}
}

func TestNewConfigRejectsBadTelegramPoll(t *testing.T) {
	t.Setenv("BOT_TELEGRAM_POLL", "nope")
	if _, err := newConfig(); err == nil {
		t.Fatal("newConfig accepted BOT_TELEGRAM_POLL=nope")
	}
	t.Setenv("BOT_TELEGRAM_POLL", "false")
	cfg, err := newConfig()
	if err != nil || cfg.TelegramPoll {
		t.Fatalf("cfg=%+v err=%v, want TelegramPoll=false", cfg, err)
	}
}
