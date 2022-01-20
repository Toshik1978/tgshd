package gammu

import (
	"context"
	"fmt"
	"math/rand"

	"golang.org/x/exp/utf8string"
)

const (
	multipartSms    = int64(0x050003) // sign of multipart message
	defaultEncoding = "Default_No_Compression"
	ucs2Encoding    = "Unicode_No_Compression"
	defaultLength   = 160
	ucs2Length      = 67
)

// builder is a builder for sequences of multipart SMS.
type builder struct {
}

// NewSequenceBuilder instantiate new builder for sequences of multipart SMS.
func NewSequenceBuilder() *builder {
	return &builder{}
}

// Do actual build multipart SMS.
func (b *builder) Do(_ context.Context, text string) []MessagePart {
	s := utf8string.NewString(text)

	// choose encoding
	coding := defaultEncoding
	length := defaultLength
	if !s.IsASCII() {
		coding = ucs2Encoding
		length = ucs2Length
	}

	// split text to parts
	parts := make([]MessagePart, 0)
	for i := 0; i < s.RuneCount(); {
		j := s.RuneCount()
		if j-i > length {
			j = i + length
		}

		parts = append(parts, MessagePart{Text: s.Slice(i, j), Coding: coding})
		i = j
	}

	// update UDH if we need it
	if len(parts) > 1 {
		b.updateUDH(parts)
	}
	return parts
}

func (b *builder) updateUDH(parts []MessagePart) {
	unique := int64(rand.Intn(256)) //nolint:gosec
	for i := 1; i <= len(parts); i++ {
		parts[i-1].UDH = b.buildUDH(unique, int64(len(parts)), int64(i))
	}
}

func (b *builder) buildUDH(unique, total, index int64) string {
	udh := (multipartSms << 24) | (unique << 16) | (total << 8) | index
	return fmt.Sprintf("%012X", udh)
}
