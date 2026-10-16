package command

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/health/report"
	"github.com/Toshik1978/tgshd/pkg/health/store"
)

// MetricsSource provides current and 7-day statistics of stored metrics.
type MetricsSource interface {
	Metrics(ctx context.Context, prefix string, now time.Time) ([]store.MetricStats, error)
}

type tempCommand struct {
	logger    *zap.Logger
	publisher Publisher
	metrics   MetricsSource
	now       func() time.Time
}

// NewTempCommand creates a new handler for the /temp command.
func NewTempCommand(logger *zap.Logger, publisher Publisher, metrics MetricsSource) *tempCommand {
	logger.Info("Temp command created")

	return &tempCommand{logger: logger, publisher: publisher, metrics: metrics, now: time.Now}
}

func (c *tempCommand) Name() string {
	return "temp"
}

func (c *tempCommand) Enabled() bool {
	return true
}

func (c *tempCommand) Handle(ctx context.Context, senderID int64, _ string) error {
	c.logger.Debug("Temp command received")

	stats, err := c.metrics.Metrics(ctx, "temp.", c.now())
	if err != nil {
		return fmt.Errorf("failed to read temperatures: %w", err)
	}
	if err := c.publisher.Publish(ctx, senderID, report.Temps(stats)); err != nil {
		return fmt.Errorf("failed to publish reply: %w", err)
	}

	return nil
}
