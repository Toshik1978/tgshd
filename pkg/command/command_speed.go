package command

import (
	"context"
	"fmt"

	"go.uber.org/zap"
)

type Speedtest interface {
	Speedtest(ctx context.Context) (float64, float64, error)
}

type speedCommand struct {
	logger    *zap.Logger
	publisher Publisher
	speedtest Speedtest
}

// NewSpeedCommand creates new handler for speed command.
func NewSpeedCommand(logger *zap.Logger, publisher Publisher, speedtest Speedtest) *speedCommand {
	logger.Info("Speed command created")
	return &speedCommand{
		logger:    logger,
		publisher: publisher,
		speedtest: speedtest,
	}
}

func (c *speedCommand) Name() string {
	return "speed"
}

func (c *speedCommand) Enabled() bool {
	return true
}

func (c *speedCommand) Handle(ctx context.Context, senderID int64, _ string) error {
	c.logger.Debug("Speed command received")

	dl, ul, err := c.speedtest.Speedtest(ctx)
	if err != nil {
		return fmt.Errorf("failed to do speedtest: %w", err)
	}

	err1 := c.publisher.Publish(
		ctx,
		senderID,
		fmt.Sprintf("Download: <b>%.2fMbps</b>\nUpload: <b>%.2fMbps</b>", dl, ul),
	)
	if err1 != nil {
		return fmt.Errorf("failed to publish reply: %w", err1)
	}
	return nil
}
