package command

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/health/report"
	"github.com/Toshik1978/tgshd/pkg/health/store"
	"github.com/Toshik1978/tgshd/pkg/telegram/route"
)

// Drive describes one known drive: Bus, Model and Serial, whether its health
// is OK, and the spare capacity and self-test age when known (nil otherwise).
type Drive = report.Drive

// DriveSource lists the known drives.
type DriveSource interface {
	Drives() []Drive
}

type disksCommand struct {
	logger    *zap.Logger
	publisher Publisher
	drives    DriveSource
	metrics   MetricsSource
	now       func() time.Time
}

// NewDisksCommand creates a new handler for the /disks command.
func NewDisksCommand(logger *zap.Logger, publisher Publisher, drives DriveSource, metrics MetricsSource) *disksCommand {
	logger.Info("Disks command created")

	return &disksCommand{logger: logger, publisher: publisher, drives: drives, metrics: metrics, now: time.Now}
}

func (c *disksCommand) Name() string {
	return "disks"
}

func (c *disksCommand) Enabled() bool {
	return true
}

func (c *disksCommand) Handle(ctx context.Context, origin route.Origin, _ string) error {
	c.logger.Debug("Disks command received")

	var stats []store.MetricStats
	drives := c.drives.Drives()
	if len(drives) > 0 {
		var err error
		if stats, err = c.driveMetrics(ctx); err != nil {
			return err
		}
	}
	if err := c.publisher.Publish(ctx, origin.Reply(), report.Disks(drives, stats)); err != nil {
		return fmt.Errorf("failed to publish reply: %w", err)
	}

	return nil
}

func (c *disksCommand) driveMetrics(ctx context.Context) ([]store.MetricStats, error) {
	now := c.now()
	temps, err := c.metrics.Metrics(ctx, "temp.drive.", now)
	if err != nil {
		return nil, fmt.Errorf("failed to read drive temperatures: %w", err)
	}
	other, err := c.metrics.Metrics(ctx, "drive.", now)
	if err != nil {
		return nil, fmt.Errorf("failed to read drive metrics: %w", err)
	}

	return append(temps, other...), nil
}
