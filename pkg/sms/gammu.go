package sms

import (
	"context"
	"errors"
	"fmt"
	"math/rand"

	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"golang.org/x/exp/utf8string"
)

const (
	multipartSms    = int64(0x050003) // sign of multipart message
	sourceID        = "Server Bot 1.0"
	defaultEncoding = "Default_No_Compression"
	ucs2Encoding    = "Unicode_No_Compression"
	defaultLength   = 160
	ucs2Length      = 67
)

var (
	errBadRequest = errors.New("phone and message can't be empty")
)

type gammu struct {
	logger *zap.Logger
	db     *sqlx.DB
}

// NewGammu instantiate wrapper for SMS sending via Gammu (DB).
func NewGammu(logger *zap.Logger, db *sqlx.DB) *gammu {
	logger.Info("Gammu wrapper created")
	return &gammu{
		logger: logger,
		db:     db,
	}
}

// Publish prepare message for Gammu and publish it via DB.
func (g *gammu) Publish(ctx context.Context, phone, msg string) error {
	if phone == "" || msg == "" {
		return errBadRequest
	}
	g.logger.With(zap.String("phone", phone)).Info("Publish SMS")

	parts := g.build(msg)
	if len(parts) == 1 {
		return g.buildOutbox(ctx, phone, parts[0])
	} else {
		return g.buildMultipartOutbox(ctx, phone, parts)
	}
}

type gammuPart struct {
	UDH    string
	Text   string
	Coding string
}

func (g *gammu) build(msg string) []gammuPart {
	s := utf8string.NewString(msg)

	coding := defaultEncoding
	length := defaultLength
	if !s.IsASCII() {
		coding = ucs2Encoding
		length = ucs2Length
	}

	parts := make([]gammuPart, 0)
	for i := 0; i < s.RuneCount(); {
		j := s.RuneCount()
		if j-i > length {
			j = i + length
		}

		parts = append(parts, gammuPart{Text: s.Slice(i, j), Coding: coding})
		i = j
	}
	if len(parts) > 1 {
		return g.updateUDH(parts)
	}
	return parts
}

func (g *gammu) updateUDH(parts []gammuPart) []gammuPart {
	unique := int64(rand.Intn(256)) //nolint:gosec
	for i := 1; i <= len(parts); i++ {
		parts[i-1].UDH = g.buildUDH(unique, int64(len(parts)), int64(i))
	}
	return parts
}

func (g *gammu) buildUDH(unique, total, index int64) string {
	udh := (multipartSms << 24) | (unique << 16) | (total << 8) | index
	return fmt.Sprintf("%012X", udh)
}
