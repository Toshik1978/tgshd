package sms

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/server-bot/pkg/zte"
)

type worker struct {
	logger    *zap.Logger
	publisher Publisher
	chatID    int64
	conn      ZTE
}

type Publisher interface {
	Publish(ctx context.Context, recipientID int64, msg string) error
}

type ZTE interface {
	ReadSms(del bool) ([]zte.Sms, error)
}

// NewWorker instantiate new SMS worker.
func NewWorker(logger *zap.Logger, publisher Publisher, chatID int64, conn ZTE) *worker {
	return &worker{
		logger:    logger,
		publisher: publisher,
		chatID:    chatID,
		conn:      conn,
	}
}

func (w *worker) Name() string {
	return "sms"
}

func (w *worker) Duration() time.Duration {
	return 15 * time.Second
}

func (w *worker) Do(ctx context.Context) error {
	sms, err := w.conn.ReadSms(true)
	errs := make([]error, 0, len(sms))
	errs = append(errs, err)

	for _, m := range sms { //nolint:gocritic
		text := "<b>SMS</b>\n\n"
		text += fmt.Sprintf("<i>From</i>: %s\n", m.Number)
		text += fmt.Sprintf("<i>Date</i>: %s\n\n", decodeDate(m.Date).Format(time.RFC1123))
		text += decodeMessage(m.Content)

		errs = append(errs, w.publisher.Publish(ctx, w.chatID, text))
	}

	return errors.Join(errs...)
}
