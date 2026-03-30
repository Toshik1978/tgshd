package command

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/Toshik1978/server-bot/pkg/zte"
)

type ZTE interface {
	SetBearer(pref zte.Bearer) error
}

type networkCommand struct {
	logger    *zap.Logger
	publisher Publisher
	conn      ZTE
	cmd       string
}

// NewNetworkCommand creates new handler for network command.
func NewNetworkCommand(logger *zap.Logger, publisher Publisher, conn ZTE, cmd string) *networkCommand {
	logger.Info("Network command created")
	return &networkCommand{
		logger:    logger,
		publisher: publisher,
		conn:      conn,
		cmd:       cmd,
	}
}

func (c *networkCommand) Name() string {
	return c.cmd
}

func (c *networkCommand) Enabled() bool {
	return true
}

func (c *networkCommand) Handle(ctx context.Context, senderID int64, _ string) error {
	c.logger.Debug("Network command received")

	if err := c.conn.SetBearer(c.commandToBearer()); err != nil {
		return fmt.Errorf("failed to set bearer: %w", err)
	}

	reply := fmt.Sprintf("Bearer preferences succeeded: <b>%s</b>", strings.ToUpper(c.cmd))
	if err := c.publisher.Publish(ctx, senderID, reply); err != nil {
		return fmt.Errorf("failed to publish reply: %w", err)
	}
	return nil
}

func (c *networkCommand) commandToBearer() zte.Bearer {
	switch c.cmd {
	case "5g":
		return zte.BearerWLAnd5G
	case "4g":
		return zte.BearerWCDMAAndLTE
	case "3g":
		return zte.BearerOnlyWCDMA
	}
	return zte.BearerWCDMAAndLTE
}
