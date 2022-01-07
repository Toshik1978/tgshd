package command

import (
	"context"
	"fmt"
	"time"

	"github.com/go-ping/ping"
	"go.uber.org/zap"

	"github.com/Toshik1978/server-bot/pkg/telegram"
)

type pingCommand struct {
	logger    *zap.Logger
	publisher telegram.Publisher
	hosts     []string
}

// NewPingCommand creates new handler for ping command.
func NewPingCommand(
	logger *zap.Logger,
	publisher telegram.Publisher,
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

func (c *pingCommand) Handle(ctx context.Context, telegramID int64, _ string) error {
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

	if _, err := c.publisher.Publish(ctx, telegramID, telegram.Message{Text: text}); err != nil {
		return fmt.Errorf("failed to publish reply: %w", err)
	}
	return nil
}
