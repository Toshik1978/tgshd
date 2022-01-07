package telegram

import (
	"context"
	"fmt"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"go.uber.org/zap"
)

// Publisher declare telegram publisher.
type Publisher interface {
	// Publish publishes message to telegram.
	Publish(ctx context.Context, recipientID int64, msg Message) (int64, error)
}

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

func (p *publisher) Publish(_ context.Context, recipientID int64, msg Message) (int64, error) {
	switch {
	case msg.Text != "":
		message := tgbotapi.NewMessage(recipientID, msg.Text)
		message.ParseMode = tgbotapi.ModeHTML
		return p.send(message)

	case msg.Document != nil:
		message := tgbotapi.NewDocument(
			recipientID,
			tgbotapi.FileBytes{
				Name:  msg.Document.Name,
				Bytes: msg.Document.Content,
			},
		)
		return p.send(message)

	case msg.Photo != nil:
		message := tgbotapi.NewPhoto(
			recipientID,
			tgbotapi.FileBytes{
				Name:  msg.Photo.Caption,
				Bytes: msg.Photo.Content,
			},
		)
		return p.send(message)

	default:
		p.logger.
			With(zap.Int64("recipient_id", recipientID)).
			Fatal("Empty telegram message to publish")
	}
	return 0, nil
}

func (p *publisher) send(what tgbotapi.Chattable) (int64, error) {
	p.logger.Info("Send telegram message")

	msg, err := p.bot.Send(what)
	if err != nil {
		return 0, fmt.Errorf("failed to send telegram message: %w", err)
	}
	return int64(msg.MessageID), nil
}
