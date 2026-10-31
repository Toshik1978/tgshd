package telegram

import (
	"context"
	"fmt"

	"go.uber.org/zap"
	"gopkg.in/telebot.v4"

	"github.com/Toshik1978/tgshd/pkg/telegram/route"
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

func (p *publisher) Publish(_ context.Context, dest route.Destination, msg string) error {
	p.logger.Debug("Send telegram message")
	_, err := p.bot.Send(&telebot.User{ID: dest.ChatID}, msg, &telebot.SendOptions{
		ParseMode:           telebot.ModeHTML,
		ThreadID:            dest.ThreadID,
		DisableNotification: dest.Silent,
	})
	if err != nil {
		return fmt.Errorf("failed to send telegram message: %w", err)
	}

	return nil
}
