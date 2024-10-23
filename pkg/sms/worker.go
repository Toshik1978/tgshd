package sms

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/server-bot/pkg/zte"
)

type worker struct {
	logger *zap.Logger
	conn   ZTE
}

type ZTE interface {
	ReadSms(del bool) ([]zte.Sms, error)
}

// NewWorker instantiate new SMS worker.
func NewWorker(logger *zap.Logger, conn ZTE) *worker {
	return &worker{
		logger: logger,
		conn:   conn,
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
