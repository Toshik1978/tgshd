package command

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/health/report"
	"github.com/Toshik1978/tgshd/pkg/health/runner"
)

// StatusSource provides the health runner state.
type StatusSource interface {
	Snapshot() runner.Snapshot
}

// MemoryStatusSource provides the /status memory block; nil leaves it out.
type MemoryStatusSource interface {
	MemoryStatus(ctx context.Context) *report.Memory
}

type statusCommand struct {
	logger    *zap.Logger
	publisher Publisher
	source    StatusSource
	memory    MemoryStatusSource
}

// NewStatusCommand creates a new handler for the /status command. memory may
// be nil: there is no memory block then.
func NewStatusCommand(
	logger *zap.Logger, publisher Publisher, source StatusSource, memory MemoryStatusSource,
) *statusCommand {
	logger.Info("Status command created")

	return &statusCommand{logger: logger, publisher: publisher, source: source, memory: memory}
}

func (c *statusCommand) Name() string {
	return "status"
}

func (c *statusCommand) Enabled() bool {
	return true
}

func (c *statusCommand) Handle(ctx context.Context, senderID int64, _ string) error {
	c.logger.Debug("Status command received")

	var mem *report.Memory
	if c.memory != nil {
		mem = c.memory.MemoryStatus(ctx)
	}
	if err := c.publisher.Publish(ctx, senderID, report.Status(c.source.Snapshot(), mem)); err != nil {
		return fmt.Errorf("failed to publish reply: %w", err)
	}

	return nil
}
