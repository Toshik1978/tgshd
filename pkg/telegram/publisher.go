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

func (p *publisher) Publish(ctx context.Context, dest route.Destination, msg string) error {
	p.logger.Debug("Send telegram message")
	err := route.SendWithFallback(ctx, dest, func(_ context.Context, d route.Destination) error {
		_, err := p.bot.Send(&telebot.User{ID: d.ChatID}, msg, &telebot.SendOptions{
			ParseMode:           telebot.ModeHTML,
			ThreadID:            d.ThreadID,
			DisableNotification: d.Silent,
		})
		if err != nil {
			return fmt.Errorf("telebot send: %w", err)
		}

		return nil
	}, func(threadID int, err error) {
		p.logger.Warn("Telegram topic rejected the message, sent without it",
			zap.Int("thread_id", threadID), zap.Error(err))
	})
	if err != nil {
		return fmt.Errorf("failed to send telegram message: %w", err)
	}

	return nil
}
