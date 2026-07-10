package gammu

import (
	"context"
	"strings"
	"testing"
)

func TestBuilderSinglePartASCII(t *testing.T) {
	parts := NewSequenceBuilder().Do(context.Background(), "hello")
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
	if parts[0].Coding != "Default_No_Compression" {
		t.Errorf("expected GSM7 coding, got %q", parts[0].Coding)
	}
	if parts[0].Text != "hello" {
		t.Errorf("expected text %q, got %q", "hello", parts[0].Text)
	}
	if parts[0].UDH != "" {
		t.Errorf("single part must have empty UDH, got %q", parts[0].UDH)
	}
}

func TestBuilderSinglePartUnicode(t *testing.T) {
	parts := NewSequenceBuilder().Do(context.Background(), "привет")
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
	if parts[0].Coding != "Unicode_No_Compression" {
		t.Errorf("expected UCS2 coding, got %q", parts[0].Coding)
	}
}

func TestBuilderMultipartASCII(t *testing.T) {
	// 200 ASCII chars > 160 GSM7 limit => 2 parts.
	body := strings.Repeat("a", 200)
	parts := NewSequenceBuilder().Do(context.Background(), body)
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	if len(parts[0].Text) != 160 || len(parts[1].Text) != 40 {
		t.Fatalf("unexpected split lengths: %d, %d", len(parts[0].Text), len(parts[1].Text))
	}
	for i, p := range parts {
		if len(p.UDH) != 12 {
			t.Fatalf("part %d: UDH must be 12 hex chars, got %q", i, p.UDH)
		}
		if p.UDH[:6] != "050003" {
			t.Errorf("part %d: UDH prefix must be 050003, got %q", i, p.UDH[:6])
		}
		if p.UDH[8:10] != "02" {
			t.Errorf("part %d: total byte must be 02, got %q", i, p.UDH[8:10])
		}
	}
	if parts[0].UDH[6:8] != parts[1].UDH[6:8] {
		t.Errorf("parts must share the same reference byte: %q vs %q", parts[0].UDH[6:8], parts[1].UDH[6:8])
	}
	if parts[0].UDH[10:12] != "01" || parts[1].UDH[10:12] != "02" {
		t.Errorf("sequence bytes wrong: %q, %q", parts[0].UDH[10:12], parts[1].UDH[10:12])
	}
}
