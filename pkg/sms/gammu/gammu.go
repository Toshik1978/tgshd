package gammu

import (
	"context"
	"errors"

	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
)

var (
	errBadRequest = errors.New("phone and message can't be empty")
)

type gammu struct {
	logger  *zap.Logger
	r       *repository
	builder *builder
}

// NewSQLBackend instantiate wrapper for SMS sending via Gammu with SQL backend.
func NewSQLBackend(logger *zap.Logger, db *sqlx.DB) *gammu {
	logger.Info("Gammu SQL backend wrapper created")
	return &gammu{
		logger:  logger,
		r:       NewRepository(logger, db),
		builder: NewSequenceBuilder(),
	}
}

// Publish prepare message for Gammu and publish it via DB.
func (g *gammu) Publish(ctx context.Context, phone, msg string) error {
	if phone == "" || msg == "" {
		return errBadRequest
	}

	g.logger.With(zap.String("phone", phone)).Info("Publish SMS")

	parts := g.builder.Do(ctx, msg)
	if len(parts) == 1 {
		return g.r.Store(ctx, phone, parts[0])
	} else {
		return g.r.StoreMultipart(ctx, phone, parts)
	}
}
