package command

import (
	"context"
	"fmt"
	"strings"

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

func (c *pingCommand) Enabled() bool {
	return len(c.hosts) > 0
}

func (c *pingCommand) Handle(ctx context.Context, senderID int64, _ string) error {
	c.logger.Debug("Ping command received")

	resp, err := c.pinger.Ping(ctx, c.hosts)
	if err != nil {
		return fmt.Errorf("failed to ping: %w", err)
	}

	var text strings.Builder
	text.WriteString("PONG\n\n")
	for _, host := range c.hosts {
		if resp[host] {
			text.WriteString(host + ": <b>OK</b>\n")
		} else {
			text.WriteString(host + ": <b>FAIL</b>\n")
		}
	}

	if err := c.publisher.Publish(ctx, senderID, text.String()); err != nil {
		return fmt.Errorf("failed to publish reply: %w", err)
	}

	return nil
}
