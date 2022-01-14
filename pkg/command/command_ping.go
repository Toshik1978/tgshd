package command

import (
	"context"
	"fmt"

	"go.uber.org/zap"
)

type Pinger interface {
	Ping(ctx context.Context, hosts []string) (map[string]bool, error)
}

type pingCommand struct {
	logger    *zap.Logger
	publisher Publisher
	pinger    Pinger
	hosts     []string
}

// NewPingCommand creates new handler for ping command.
func NewPingCommand(logger *zap.Logger, publisher Publisher, pinger Pinger, hosts []string) *pingCommand {
	logger.Info("Ping command created")
	return &pingCommand{
		logger:    logger,
		publisher: publisher,
		pinger:    pinger,
		hosts:     hosts,
	}
}

func (c *pingCommand) Name() string {
	return "ping"
}

func (c *pingCommand) Handle(ctx context.Context, senderID int64) error {
	resp, err := c.pinger.Ping(ctx, c.hosts)
	if err != nil {
		return fmt.Errorf("failed to ping: %w", err)
	}

	text := "PONG\n\n"
	for _, host := range c.hosts {
		if resp[host] {
			text += host + ": OK\n"
		} else {
			text += host + ": FAIL\n"
		}
	}

	if err := c.publisher.Publish(ctx, senderID, text); err != nil {
		return fmt.Errorf("failed to publish reply: %w", err)
	}
	return nil
}
