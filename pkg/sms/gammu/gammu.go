package gammu

import (
	"context"
	"database/sql"
	"errors"

	"go.uber.org/zap"
)

var errBadRequest = errors.New("phone and message can't be empty")

type gammu struct {
	logger  *zap.Logger
	r       *repository
	builder *builder
}

// NewSQLBackend instantiates the gammu SMS sender backed by a SQL outbox.
func NewSQLBackend(logger *zap.Logger, db *sql.DB) *gammu {
	logger.Info("Gammu SQL backend wrapper created")

	return &gammu{
		logger:  logger,
		r:       NewRepository(logger, db),
		builder: NewSequenceBuilder(),
	}
}

// Publish builds the message parts and stores them in the gammu outbox for delivery.
func (g *gammu) Publish(ctx context.Context, phone, msg string) error {
	if phone == "" || msg == "" {
		return errBadRequest
	}
	g.logger.With(zap.String("phone", phone)).Info("Publish SMS")

	parts := g.builder.Do(ctx, msg)
	if len(parts) == 1 {
		return g.r.Store(ctx, phone, parts[0])
	}

	return g.r.StoreMultipart(ctx, phone, parts)
}
