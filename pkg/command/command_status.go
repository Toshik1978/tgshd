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

type statusCommand struct {
	logger    *zap.Logger
	publisher Publisher
	source    StatusSource
}

// NewStatusCommand creates a new handler for the /status command.
func NewStatusCommand(logger *zap.Logger, publisher Publisher, source StatusSource) *statusCommand {
	logger.Info("Status command created")

	return &statusCommand{logger: logger, publisher: publisher, source: source}
}

func (c *statusCommand) Name() string {
	return "status"
}

func (c *statusCommand) Enabled() bool {
	return true
}

func (c *statusCommand) Handle(ctx context.Context, senderID int64, _ string) error {
	c.logger.Debug("Status command received")

	if err := c.publisher.Publish(ctx, senderID, report.Status(c.source.Snapshot())); err != nil {
		return fmt.Errorf("failed to publish reply: %w", err)
	}

	return nil
}
