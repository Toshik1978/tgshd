package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.uber.org/zap"
	"gopkg.in/telebot.v4"
)

// Handler define handler for specific command.
type Handler interface {
	// Name returns name of command to handle.
	Name() string
	// Enabled return true if command is enabled.
	Enabled() bool
	// Handle handles command.
	Handle(ctx context.Context, senderID int64, cmd string) error
}

type consumer struct {
	logger   *zap.Logger
	bot      *telebot.Bot
	handlers []Handler
}

// NewConsumer initializes new consumer for telegram messages.
func NewConsumer(logger *zap.Logger, bot *telebot.Bot, handlers []Handler) *consumer {
	logger.Info("Creation of TelegramConsumer")

	return &consumer{
		logger:   logger,
		bot:      bot,
		handlers: handlers,
	}
}

func (c *consumer) Start(ctx context.Context) error {
	c.logger.Info("Telegram bot started")

	// Add commands.
	enabled := make(map[string]Handler)
	for _, handler := range c.handlers {
		if handler.Enabled() {
			names := strings.SplitSeq(handler.Name(), "|")
			for name := range names {
				c.logger.With(zap.String("command", name)).Info("Telegram command added")
				c.bot.Handle("/"+name, func(tctx telebot.Context) error {
					return c.handleCommand(ctx, handler, tctx)
				})
				enabled["/"+name] = handler
			}
		}
	}
	// Add generic handler in case we are in the channel.
	c.bot.Handle(telebot.OnChannelPost, func(tctx telebot.Context) error {
		return c.handleCommands(ctx, enabled, tctx)
	})

	go c.bot.Start()

	return nil
}

func (c *consumer) Stop(ctx context.Context) error {
	shutdownChannel := make(chan any)
	go func() {
		c.bot.Stop()
		close(shutdownChannel)
		c.logger.Info("Telegram bot gracefully stopped")
	}()

	select {
	case <-shutdownChannel:
	case <-ctx.Done():
		return context.DeadlineExceeded
	}

	return nil
}

func (c *consumer) handleCommands(ctx context.Context, handlers map[string]Handler, tctx telebot.Context) error {
	fields := strings.Fields(tctx.Text())
	if len(fields) == 0 {
		return nil
	}
	if handler, ok := handlers[fields[0]]; ok {
		return c.handleCommand(ctx, handler, tctx)
	}

	return nil
}

func (c *consumer) handleCommand(ctx context.Context, handler Handler, tctx telebot.Context) error {
	logger := c.logger.With(zap.String("command", handler.Name()))
	message := tctx.Message()

	var senderID int64
	switch {
	case message.Sender != nil:
		senderID = message.Sender.ID
	case message.Chat != nil:
		senderID = message.Chat.ID
	default:
		logger.Error("Failed to get sender ID")
		return errors.New("failed to get sender ID")
	}

	logger = logger.With(zap.Int64("sender_id", senderID))
	logger.Debug("Handle command")

	defer func() {
		if p := recover(); p != nil {
			logger.With(zap.Any("panic", p)).Error("Panic in message handler")
		}
	}()

	err := handler.Handle(ctx, senderID, tctx.Text())
	if err != nil {
		logger.
			With(zap.Error(err)).
			Error("Error in message handler")
		return fmt.Errorf("failed to handle command: %w", err)
	}

	return nil
}
