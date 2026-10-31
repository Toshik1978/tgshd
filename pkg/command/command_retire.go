package command

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/health/report"
	"github.com/Toshik1978/tgshd/pkg/health/runner"
	"github.com/Toshik1978/tgshd/pkg/telegram/route"
)

// Retirer retires a drive by serial or serial suffix; the health runner
// satisfies it.
type Retirer interface {
	Retire(ctx context.Context, serial string) (runner.RetireResult, error)
}

type retireCommand struct {
	logger    *zap.Logger
	publisher Publisher
	retirer   Retirer
}

// NewRetireCommand creates a new handler for the /retire command.
func NewRetireCommand(logger *zap.Logger, publisher Publisher, retirer Retirer) *retireCommand {
	logger.Info("Retire command created")

	return &retireCommand{logger: logger, publisher: publisher, retirer: retirer}
}

func (c *retireCommand) Name() string {
	return "retire"
}

func (c *retireCommand) Enabled() bool {
	return true
}

func (c *retireCommand) Handle(ctx context.Context, origin route.Origin, cmd string) error {
	arg := ""
	if fields := strings.Fields(cmd); len(fields) > 1 {
		arg = fields[1]
	}

	res, err := c.retirer.Retire(ctx, arg)
	var reply string
	switch {
	case err == nil:
		c.logger.Info("Drive retired by command", zap.String("drive", res.Drive.Label()))
		reply = report.Retired(res.Drive, res.Cleared)
	case errors.Is(err, runner.ErrAmbiguousDrive):
		reply = report.Drives(
			"Ambiguous drive; use more of the serial, or the full serial. Matching drives:",
			res.Candidates,
		)
	case errors.Is(err, runner.ErrNoDrive):
		heading := "Unknown drive. Known drives:"
		if arg == "" {
			heading = "Usage: /retire <serial or last 4+ chars>. Known drives:"
		}
		reply = report.Drives(heading, res.Candidates)
	default:
		return fmt.Errorf("failed to retire drive: %w", err)
	}

	if err := c.publisher.Publish(ctx, origin.Reply(), reply); err != nil {
		return fmt.Errorf("failed to publish reply: %w", err)
	}

	return nil
}
