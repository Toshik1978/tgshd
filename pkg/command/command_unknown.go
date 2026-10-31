package command

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/telegram/route"
)

// scriptNameTimeout bounds the one-shot script invocation used to resolve the
// command list at construction, so a hanging operator script cannot wedge boot.
const scriptNameTimeout = 10 * time.Second

type Script interface {
	List(ctx context.Context) (string, error)
	Execute(ctx context.Context, cmd string, threadID int) error
}

type unknownCommand struct {
	logger    *zap.Logger
	publisher Publisher
	runner    Script
	commands  []route.BotCommand
}

// NewUnknownCommand creates new handler for unknown command. The command list
// is resolved from the script once, here at construction, so Commands() stays a
// cheap accessor instead of re-executing the script on every dispatch.
func NewUnknownCommand(logger *zap.Logger, publisher Publisher, runner Script) (*unknownCommand, error) {
	ctx, cancel := context.WithTimeout(context.Background(), scriptNameTimeout)
	defer cancel()

	out, err := runner.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get unknown commands list: %w", err)
	}

	commands, problems := parseScriptCommands(out)
	for _, problem := range problems {
		logger.Warn(problem)
	}

	logger.Info("Unknown command created", zap.Int("commands", len(commands)))

	return &unknownCommand{
		logger:    logger,
		publisher: publisher,
		runner:    runner,
		commands:  commands,
	}, nil
}

// Commands returns the entries parsed from the script's command list.
func (c *unknownCommand) Commands() []route.BotCommand {
	return c.commands
}

// CustomScript marks this as the custom-script handler, so the consumer can
// register its commands after the built-ins.
func (c *unknownCommand) CustomScript() {}

func (c *unknownCommand) Enabled() bool {
	return true
}

func (c *unknownCommand) Handle(ctx context.Context, origin route.Origin, cmd string) error {
	c.logger.Debug("Unknown command received")

	if err := c.runner.Execute(ctx, cmd, origin.ThreadID); err != nil {
		return fmt.Errorf("failed to run script: %w", err)
	}

	if err := c.publisher.Publish(ctx, origin.Reply(), "Script succeeded!"); err != nil {
		return fmt.Errorf("failed to publish reply: %w", err)
	}

	return nil
}
