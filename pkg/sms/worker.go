package sms

import (
	"context"
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
	return nil
}
