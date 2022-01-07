package command

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/Toshik1978/server-bot/pkg/telegram"
)

// service declare command service.
type service struct {
	logger    *zap.Logger
	publisher telegram.Publisher
	handlers  map[string]Handler
}

// NewService instantiate new command service.
func NewService(
	logger *zap.Logger,
	publisher telegram.Publisher,
	handlers []Handler,
) *service {
	logger.
		With(zap.Int("commands_count", len(handlers))).
		Info("Creation of CommandService")

	return &service{
		logger:    logger,
		publisher: publisher,
		handlers:  prepareHandlers(handlers),
	}
}

func prepareHandlers(handlers []Handler) map[string]Handler {
	mapping := make(map[string]Handler)
	for _, handler := range handlers {
		mapping[strings.ToLower(handler.Name())] = handler
	}
	return mapping
}

func (s *service) On(ctx context.Context, telegramID int64, cmd, args string) error {
	handler := s.handlers[strings.ToLower(cmd)]
	if handler == nil {
		return fmt.Errorf("unsupported command received: %s", cmd)
	}

	if err := handler.Handle(ctx, telegramID, args); err != nil {
		return fmt.Errorf("failed to handle command: %w", err)
	}
	return nil
}
