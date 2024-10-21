package command

import (
	"context"
	"fmt"

	"go.uber.org/zap"
)

type ZTE interface {
	Login() error
	Logout() error
}

type networkCommand struct {
	logger    *zap.Logger
	publisher Publisher
	conn      ZTE
}

// NewNetworkCommand creates new handler for network command.
func NewNetworkCommand(logger *zap.Logger, publisher Publisher, conn ZTE) *networkCommand {
	logger.Info("Network command created")
	return &networkCommand{
		logger:    logger,
		publisher: publisher,
		conn:      conn,
	}
}

func (c *networkCommand) Name() string {
	return "network"
}

func (c *networkCommand) Enabled() bool {
	return true
}

func (c *networkCommand) Handle(ctx context.Context, senderID int64) error {
	c.logger.Debug("Network command received")

	err := c.conn.Login()
	if err != nil {
		return fmt.Errorf("failed to login: %w", err)
	}

	err = c.conn.Logout()
	if err != nil {
		return fmt.Errorf("failed to logout: %w", err)
	}

	text := "CONNECTION SUCCEEDED"
	if err := c.publisher.Publish(ctx, senderID, text); err != nil {
		return fmt.Errorf("failed to publish reply: %w", err)
	}
	return nil
}
