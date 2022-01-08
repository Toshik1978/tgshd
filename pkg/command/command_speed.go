package command

import (
	"context"
	"fmt"

	"github.com/showwin/speedtest-go/speedtest"
	"go.uber.org/zap"
)

type speedCommand struct {
	logger    *zap.Logger
	publisher Publisher
}

// NewSpeedCommand creates new handler for speed command.
func NewSpeedCommand(
	logger *zap.Logger,
	publisher Publisher,
) *speedCommand {
	logger.Info("Speed command created")
	return &speedCommand{
		logger:    logger,
		publisher: publisher,
	}
}

func (c *speedCommand) Name() string {
	return "speed"
}

func (c *speedCommand) Handle(ctx context.Context, senderID int64) error {
	server, err := c.findServer(ctx)
	if err != nil {
		return fmt.Errorf("failed to get best server for testing: %w", err)
	}

	if err := server.DownloadTestContext(ctx, true); err != nil {
		return fmt.Errorf("failed to do download test: %w", err)
	}
	if err := server.UploadTestContext(ctx, true); err != nil {
		return fmt.Errorf("failed to do upload test: %w", err)
	}

	text := fmt.Sprintf("Download: %.2f\nUpload: %.2f", server.DLSpeed, server.ULSpeed)
	if err := c.publisher.Publish(ctx, senderID, text); err != nil {
		return fmt.Errorf("failed to publish reply: %w", err)
	}
	return nil
}

func (c *speedCommand) findServer(ctx context.Context) (*speedtest.Server, error) {
	user, err := speedtest.FetchUserInfoContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch user info: %w", err)
	}
	servers, err := speedtest.FetchServerListContext(ctx, user)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch servers list: %w", err)
	}
	targets, err := servers.FindServer([]int{})
	if err != nil {
		return nil, fmt.Errorf("failed to get server for testing: %w", err)
	}
	if len(targets) != 1 {
		return nil, fmt.Errorf("failed to get server for testing: %w", err)
	}
	return targets[0], nil
}
