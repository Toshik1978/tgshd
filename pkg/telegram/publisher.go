package telegram

import (
	"context"
	"fmt"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"go.uber.org/zap"
)

type publisher struct {
	logger *zap.Logger
	bot    *tgbotapi.BotAPI
}

// NewPublisher initializes new publisher for telegram messages.
func NewPublisher(logger *zap.Logger, bot *tgbotapi.BotAPI) *publisher {
	logger.Info("Creation of TelegramPublisher")
	return &publisher{
		logger: logger,
		bot:    bot,
	}
}

func (p *publisher) Publish(_ context.Context, recipientID int64, msg string) error {
	p.logger.Info("Send telegram message")
	message := tgbotapi.NewMessage(recipientID, msg)
	message.ParseMode = tgbotapi.ModeHTML

	if _, err := p.bot.Send(message); err != nil {
		return fmt.Errorf("failed to send telegram message: %w", err)
	}
	return nil
}
