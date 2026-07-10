package command

import (
	"context"
	"fmt"

	"go.uber.org/zap"
)

type Script interface {
	Name() (string, error)
	Execute(ctx context.Context, cmd string) error
}

type unknownCommand struct {
	logger    *zap.Logger
	publisher Publisher
	runner    Script
}

// NewUnknownCommand creates new handler for unknown command.
func NewUnknownCommand(logger *zap.Logger, publisher Publisher, runner Script) *unknownCommand {
	logger.Info("Unknown command created")

	return &unknownCommand{
		logger:    logger,
		publisher: publisher,
		runner:    runner,
	}
}

func (c *unknownCommand) Name() string {
	name, err := c.runner.Name()
	if err != nil {
		c.logger.Panic("Failed to get unknown commands list", zap.Error(err))
	}
	return name
}

func (c *unknownCommand) Enabled() bool {
	return true
}

func (c *unknownCommand) Handle(ctx context.Context, senderID int64, cmd string) error {
	c.logger.Debug("Unknown command received")

	if err := c.runner.Execute(ctx, cmd); err != nil {
		return fmt.Errorf("failed to run script: %w", err)
	}

	if err := c.publisher.Publish(ctx, senderID, "Script succeeded!"); err != nil {
		return fmt.Errorf("failed to publish reply: %w", err)
	}

	return nil
}
