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
	name      string
}

// NewUnknownCommand creates new handler for unknown command. The command list
// is resolved from the script once, here at construction, so Name() stays a
// cheap accessor instead of re-executing the script on every dispatch.
func NewUnknownCommand(logger *zap.Logger, publisher Publisher, runner Script) (*unknownCommand, error) {
	name, err := runner.Name()
	if err != nil {
		return nil, fmt.Errorf("failed to get unknown commands list: %w", err)
	}

	logger.Info("Unknown command created")

	return &unknownCommand{
		logger:    logger,
		publisher: publisher,
		runner:    runner,
		name:      name,
	}, nil
}

func (c *unknownCommand) Name() string {
	return c.name
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
