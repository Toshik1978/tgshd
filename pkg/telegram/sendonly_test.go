package telegram

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestSendOnlyConsumer(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	c := NewSendOnlyConsumer(zap.New(core))

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	entries := logs.FilterLevelExact(zap.InfoLevel).All()
	var found int
	for _, e := range entries {
		if e.Message == "Telegram send-only mode: no command polling" {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("want one send-only info line, got %d (entries: %v)", found, entries)
	}
}
