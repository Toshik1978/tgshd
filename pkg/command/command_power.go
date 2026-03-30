package command

import (
	"context"
	"fmt"

	"go.uber.org/zap"
)

type Power interface {
	Valid() bool
	Voltage(ctx context.Context) (float64, error)
}

type powerCommand struct {
	logger    *zap.Logger
	publisher Publisher
	power     Power
}

// NewPowerCommand creates new handler for power command.
func NewPowerCommand(logger *zap.Logger, publisher Publisher, power Power) *powerCommand {
	logger.Info("Power command created")
	return &powerCommand{
		logger:    logger,
		publisher: publisher,
		power:     power,
	}
}

func (c *powerCommand) Name() string {
	return "power"
}

func (c *powerCommand) Enabled() bool {
	return c.power.Valid()
}

func (c *powerCommand) Handle(ctx context.Context, senderID int64, _ string) error {
	c.logger.Debug("Power command received")

	voltage, err := c.power.Voltage(ctx)
	if err != nil {
		return fmt.Errorf("failed to get voltage: %w", err)
	}

	if err := c.publisher.Publish(ctx, senderID, fmt.Sprintf("Voltage: <b>%.1f</b>", voltage)); err != nil {
		return fmt.Errorf("failed to publish reply: %w", err)
	}
	return nil
}
