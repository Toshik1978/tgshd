package telegram

import (
	"context"
	"fmt"

	"go.uber.org/zap"
	"gopkg.in/telebot.v4"
)

type publisher struct {
	logger *zap.Logger
	bot    *telebot.Bot
}

// NewPublisher initializes new publisher for telegram messages.
func NewPublisher(logger *zap.Logger, bot *telebot.Bot) *publisher {
	logger.Info("Creation of TelegramPublisher")
	return &publisher{
		logger: logger,
		bot:    bot,
	}
}

func (p *publisher) Publish(_ context.Context, recipientID int64, msg string) error {
	p.logger.Debug("Send telegram message")
	_, err := p.bot.Send(&telebot.User{ID: recipientID}, msg, &telebot.SendOptions{
		ParseMode: telebot.ModeHTML,
	})
	if err != nil {
		return fmt.Errorf("failed to send telegram message: %w", err)
	}
	return nil
}
