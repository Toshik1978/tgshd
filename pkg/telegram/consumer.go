package telegram

import (
	"context"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"go.uber.org/zap"
)

// Callback define callback for command handler.
type Callback interface {
	// OnCommand handles command.
	OnCommand(ctx context.Context, senderID int64, command string) error
}

// Consumer declare telegram consumer.
type Consumer interface {
	// Start starts consuming messages.
	Start(ctx context.Context) error
	// Stop stops consuming messages.
	Stop(ctx context.Context) error
}

type consumer struct {
	logger          *zap.Logger
	bot             *tgbotapi.BotAPI
	cb              Callback
	shutdownChannel chan interface{}
}

// NewConsumer initializes new consumer for telegram messages.
func NewConsumer(logger *zap.Logger, bot *tgbotapi.BotAPI, cb Callback) *consumer {
	logger.Info("Creation of TelegramConsumer")
	return &consumer{
		logger:          logger,
		bot:             bot,
		cb:              cb,
		shutdownChannel: make(chan interface{}),
	}
}

func (c *consumer) Start(ctx context.Context) error {
	c.logger.Info("Telegram bot started")

	go func() {
		c.handleCommands()
		close(c.shutdownChannel)
		c.logger.Info("Telegram bot stopped")
	}()
	return nil
}

func (c *consumer) handleCommands() {
	updates := c.bot.GetUpdatesChan(tgbotapi.NewUpdate(0))
	for update := range updates {
		if update.Message != nil {
			c.handleCommand(update.Message)
		} else if update.ChannelPost != nil {
			c.handleCommand(update.ChannelPost)
		}
	}
}

func (c *consumer) handleCommand(message *tgbotapi.Message) {
	command := message.Command()
	logger := c.logger.With(zap.String("command", command))

	var senderID int64
	switch {
	case message.From != nil:
		senderID = message.From.ID
	case message.Chat != nil:
		senderID = message.Chat.ID
	default:
		logger.Error("Failed to get sender ID")
		return
	}

	logger = logger.With(zap.Int64("sender_id", senderID))
	logger.Info("Handle command")

	defer func() {
		if p := recover(); p != nil {
			logger.With(zap.Any("panic", p)).Error("Panic in message handler")
		}
	}()

	err := c.cb.OnCommand(context.Background(), senderID, command)
	if err != nil {
		logger.
			With(zap.Error(err)).
			Error("Error in message handler")
		return
	}
}

func (c *consumer) Stop(ctx context.Context) error {
	c.bot.StopReceivingUpdates()

	select {
	case <-c.shutdownChannel:
	case <-ctx.Done():
		return context.DeadlineExceeded
	}
	return nil
}
