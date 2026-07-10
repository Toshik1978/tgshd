package gammu

import (
	"context"
	"fmt"
	"math/rand/v2"
)

const (
	multipartSMS    = int64(0x050003) // concatenated-SMS UDH signature
	defaultEncoding = "Default_No_Compression"
	ucs2Encoding    = "Unicode_No_Compression"
	defaultLength   = 160
	ucs2Length      = 67
)

// builder splits a message body into SMS parts.
type builder struct{}

// NewSequenceBuilder instantiates a new SMS sequence builder.
func NewSequenceBuilder() *builder {
	return &builder{}
}

// Do splits text into one or more message parts, choosing GSM7 or UCS2 encoding
// and attaching multipart UDH headers when more than one part is produced.
func (b *builder) Do(_ context.Context, text string) []MessagePart {
	runes := []rune(text)

	coding := defaultEncoding
	length := defaultLength
	if !isASCII(text) {
		coding = ucs2Encoding
		length = ucs2Length
	}

	parts := make([]MessagePart, 0)
	for i := 0; i < len(runes); {
		j := len(runes)
		if j-i > length {
			j = i + length
		}
		parts = append(parts, MessagePart{Text: string(runes[i:j]), Coding: coding})
		i = j
	}

	if len(parts) > 1 {
		b.updateUDH(parts)
	}
	return parts
}

func isASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}

func (b *builder) updateUDH(parts []MessagePart) {
	reference := int64(rand.IntN(256))
	total := int64(len(parts))
	for i := range parts {
		parts[i].UDH = b.buildUDH(reference, total, int64(i+1))
	}
}

func (b *builder) buildUDH(reference, total, index int64) string {
	udh := (multipartSMS << 24) | (reference << 16) | (total << 8) | index
	return fmt.Sprintf("%012X", udh)
}
