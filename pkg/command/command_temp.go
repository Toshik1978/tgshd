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

// MetricsSource provides current and 7-day statistics of stored metrics.
type MetricsSource interface {
	Metrics(ctx context.Context, prefix string, now time.Time) ([]store.MetricStats, error)
}

// TempSensorSource provides the temperature sensors the check knows, with
// their limits and unreadable times.
type TempSensorSource interface {
	TempSensors() []report.Sensor
}

type tempCommand struct {
	logger    *zap.Logger
	publisher Publisher
	metrics   MetricsSource
	sensors   TempSensorSource
	now       func() time.Time
}

// NewTempCommand creates a new handler for the /temp command. sensors may be
// nil: the reply then lists the stored metrics without limits.
func NewTempCommand(
	logger *zap.Logger, publisher Publisher, metrics MetricsSource, sensors TempSensorSource,
) *tempCommand {
	logger.Info("Temp command created")

	return &tempCommand{logger: logger, publisher: publisher, metrics: metrics, sensors: sensors, now: time.Now}
}

func (c *tempCommand) Name() string {
	return "temp"
}

func (c *tempCommand) Enabled() bool {
	return true
}

func (c *tempCommand) Handle(ctx context.Context, origin route.Origin, _ string) error {
	c.logger.Debug("Temp command received")

	stats, err := c.metrics.Metrics(ctx, "temp.", c.now())
	if err != nil {
		return fmt.Errorf("failed to read temperatures: %w", err)
	}
	var sensors []report.Sensor
	if c.sensors != nil {
		sensors = c.sensors.TempSensors()
	}
	if err := c.publisher.Publish(ctx, origin.Reply(), report.Temps(stats, sensors)); err != nil {
		return fmt.Errorf("failed to publish reply: %w", err)
	}

	return nil
}
