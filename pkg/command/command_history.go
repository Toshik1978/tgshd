package command

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/health/report"
	"github.com/Toshik1978/tgshd/pkg/health/store"
	"github.com/Toshik1978/tgshd/pkg/telegram/route"
)

// HistorySource reads the alert log; the state store satisfies it.
type HistorySource interface {
	Alerts(ctx context.Context, filter string, since time.Time, limit int) ([]store.Alert, error)
}

type historyCommand struct {
	logger    *zap.Logger
	publisher Publisher
	source    HistorySource
}

// NewHistoryCommand creates a new handler for the /history command.
func NewHistoryCommand(logger *zap.Logger, publisher Publisher, source HistorySource) *historyCommand {
	logger.Info("History command created")

	return &historyCommand{logger: logger, publisher: publisher, source: source}
}

func (c *historyCommand) Name() string {
	return "history"
}

func (c *historyCommand) Enabled() bool {
	return true
}

func (c *historyCommand) Handle(ctx context.Context, origin route.Origin, cmd string) error {
	args, ok := parseHistoryArgs(strings.Fields(cmd))

	var reply string
	if !ok {
		reply = report.HistoryUsage()
	} else {
		now := time.Now()
		// The limit is deliberately unbounded: report.History truncates the reply.
		alerts, err := c.source.Alerts(ctx, args.filter, now.Add(-args.window), 0)
		if err != nil {
			return fmt.Errorf("failed to read alert history: %w", err)
		}
		reply = report.History(alerts, args.period, now.Location())
	}

	if err := c.publisher.Publish(ctx, origin.Reply(), reply); err != nil {
		return fmt.Errorf("failed to publish reply: %w", err)
	}

	return nil
}

// periodLike matches arguments that look like a period, such as "5d".
var periodLike = regexp.MustCompile(`^\d+[hdHD]$`)

type historyArgs struct {
	filter, period string
	window         time.Duration
}

// parseHistoryArgs splits "/history [check] [period]". A single argument that
// is a valid period is the period; one that merely looks like a period is
// rejected (condition keys contain "/", check names never look like that);
// anything else is the check filter.
func parseHistoryArgs(fields []string) (historyArgs, bool) {
	args := fields[min(1, len(fields)):]
	def, _ := report.ParsePeriod(report.DefaultPeriod)
	switch len(args) {
	case 0:
		return historyArgs{"", report.DefaultPeriod, def}, true
	case 1:
		if d, isPeriod := report.ParsePeriod(args[0]); isPeriod {
			return historyArgs{"", args[0], d}, true
		}
		if periodLike.MatchString(args[0]) {
			return historyArgs{}, false
		}

		return historyArgs{args[0], report.DefaultPeriod, def}, true
	case 2:
		d, valid := report.ParsePeriod(args[1])

		return historyArgs{args[0], args[1], d}, valid
	}

	return historyArgs{}, false
}
