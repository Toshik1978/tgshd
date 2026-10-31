package command

import (
	"context"
	"fmt"
	"html"
	"strings"

	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/health/report"
	"github.com/Toshik1978/tgshd/pkg/telegram/route"
)

// Clearer clears an open condition without sending RESOLVED and lists the
// open ones; the health runner satisfies it together with StatusSource.
type Clearer interface {
	StatusSource
	Clear(key string) bool
}

type clearCommand struct {
	logger    *zap.Logger
	publisher Publisher
	clearer   Clearer
}

// NewClearCommand creates a new handler for the /clear command.
func NewClearCommand(logger *zap.Logger, publisher Publisher, clearer Clearer) *clearCommand {
	logger.Info("Clear command created")

	return &clearCommand{logger: logger, publisher: publisher, clearer: clearer}
}

func (c *clearCommand) Name() string {
	return "clear"
}

func (c *clearCommand) Enabled() bool {
	return true
}

func (c *clearCommand) Handle(ctx context.Context, origin route.Origin, cmd string) error {
	key := ""
	if fields := strings.Fields(cmd); len(fields) > 1 {
		key = fields[1]
	}

	var reply string
	if key != "" && c.clearer.Clear(key) {
		c.logger.With(zap.String("key", key)).Info("Condition cleared by command")
		reply = "Cleared " + html.EscapeString(key) + " (no RESOLVED sent)."
	} else {
		open := c.clearer.Snapshot().Open
		keys := make([]string, 0, len(open))
		for _, o := range open {
			keys = append(keys, o.Key)
		}
		heading := "Usage: /clear <condition key>. Open conditions:"
		if key != "" {
			heading = "Unknown condition " + key + ". Open conditions:"
		}
		reply = report.OpenKeys(heading, keys)
	}

	if err := c.publisher.Publish(ctx, origin.Reply(), reply); err != nil {
		return fmt.Errorf("failed to publish reply: %w", err)
	}

	return nil
}
