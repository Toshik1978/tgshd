package command

import (
	"context"
	"fmt"
	"time"

	"github.com/go-ping/ping"
	"go.uber.org/zap"
)

type pingCommand struct {
	logger    *zap.Logger
	publisher Publisher
	hosts     []string
}

// NewPingCommand creates new handler for ping command.
func NewPingCommand(
	logger *zap.Logger,
	publisher Publisher,
	hosts []string,
) *pingCommand {
	logger.Info("Ping command created")
	return &pingCommand{
		logger:    logger,
		publisher: publisher,
		hosts:     hosts,
	}
}

func (c *pingCommand) Name() string {
	return "ping"
}

func (c *pingCommand) Handle(ctx context.Context, senderID int64) error {
	text := "PONG\n\n"
	for _, host := range c.hosts {
		pinger, err := ping.NewPinger(host)
		if err != nil {
			return fmt.Errorf("failed to ping: %w", err)
		}
		pinger.Count = 3
		pinger.Timeout = 5 * time.Second

		if err := pinger.Run(); err != nil {
			return fmt.Errorf("failed to ping: %w", err)
		}

		statistics := pinger.Statistics()
		if statistics.PacketsSent == statistics.PacketsRecv {
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
