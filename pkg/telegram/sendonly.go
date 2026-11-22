package telegram

import (
	"context"

	"go.uber.org/zap"
)

type sendOnlyConsumer struct {
	logger *zap.Logger
}

// NewSendOnlyConsumer returns a consumer that never polls Telegram, so the
// daemon can only send messages (BOT_TELEGRAM_POLL=false).
func NewSendOnlyConsumer(logger *zap.Logger) *sendOnlyConsumer {
	return &sendOnlyConsumer{logger: logger}
}

// Start logs the mode and returns without any Telegram call.
func (c *sendOnlyConsumer) Start(context.Context) error {
	c.logger.Info("Telegram send-only mode: no command polling")
	return nil
}

// Stop is a no-op.
func (c *sendOnlyConsumer) Stop(context.Context) error {
	return nil
}
