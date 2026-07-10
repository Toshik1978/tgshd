package sms

import (
	"testing"
	"time"
)

func TestDecodeMessage(t *testing.T) {
	tests := []struct {
		name    string
		encoded string
		want    string
	}{
		{
			name:    "empty string",
			encoded: "",
			want:    "",
		},
		{
			name:    "single ascii char",
			encoded: "0041",
			want:    "A",
		},
		{
			name:    "ascii word",
			encoded: "00480065006C006C006F",
			want:    "Hello",
		},
		{
			name:    "ignore char tab only",
			encoded: "0009",
			want:    "",
		},
		{
			name:    "ignore char null only",
			encoded: "0000",
			want:    "",
		},
		{
			name:    "ignore char mixed with content",
			encoded: "0009004100420009",
			want:    "AB",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decodeMessage(tt.encoded)
			if got != tt.want {
				t.Errorf("decodeMessage(%q) = %q, want %q", tt.encoded, got, tt.want)
			}
		})
	}
}

func TestDecodeDate(t *testing.T) {
	got := decodeDate("2024,1,2,3,4,5")

	if got.Year() != 2024 {
		t.Errorf("Year() = %d, want 2024", got.Year())
	}
	if got.Month() != time.January {
		t.Errorf("Month() = %v, want %v", got.Month(), time.January)
	}
	if got.Day() != 2 {
		t.Errorf("Day() = %d, want 2", got.Day())
	}
	if got.Hour() != 3 {
		t.Errorf("Hour() = %d, want 3", got.Hour())
	}
	if got.Minute() != 4 {
		t.Errorf("Minute() = %d, want 4", got.Minute())
	}
	if got.Second() != 5 {
		t.Errorf("Second() = %d, want 5", got.Second())
	}
	if got.Location() != time.Local {
		t.Errorf("Location() = %v, want %v", got.Location(), time.Local)
	}
}

func TestHex2Char(t *testing.T) {
	tests := []struct {
		name string
		hex  string
		want string
	}{
		{
			name: "bmp code point A",
			hex:  "0041",
			want: "A",
		},
		{
			name: "bmp code point digit",
			hex:  "0030",
			want: "0",
		},
		{
			name: "invalid hex string",
			hex:  "zzzz",
			want: "",
		},
		{
			name: "empty string",
			hex:  "",
			want: "",
		},
		{
			name: "supplementary plane code point produces surrogate pair branch",
			hex:  "1F600",
			// utf16.Encode rejects the manually-built surrogate-range rune
			// and substitutes the Unicode replacement character (U+FFFD),
			// whose decimal code point is 65533 — this is the actual,
			// slightly surprising behavior of the current implementation.
			want: "65533",
		},
		{
			name: "code point beyond valid Unicode range",
			hex:  "110000",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hex2Char(tt.hex)
			if got != tt.want {
				t.Errorf("hex2Char(%q) = %q, want %q", tt.hex, got, tt.want)
			}
		})
	}
}

func TestEncodeMessage(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want string
	}{
		{
			name: "empty string",
			msg:  "",
			want: "",
		},
		{
			name: "single ascii char",
			msg:  "A",
			want: "0041",
		},
		{
			name: "ascii word",
			msg:  "Hello",
			want: "00480065006c006c006f",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := encodeMessage(tt.msg)
			if got != tt.want {
				t.Errorf("encodeMessage(%q) = %q, want %q", tt.msg, got, tt.want)
			}
		})
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	msg := "Hello"
	encoded := encodeMessage(msg)
	decoded := decodeMessage(encoded)
	if decoded != msg {
		t.Errorf("round trip: got %q, want %q", decoded, msg)
	}
}

func TestDec2Hex(t *testing.T) {
	tests := []struct {
		name string
		a    int64
		want string
	}{
		{
			name: "zero",
			a:    0,
			want: "0",
		},
		{
			name: "255",
			a:    255,
			want: "ff",
		},
		{
			name: "65",
			a:    65,
			want: "41",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dec2Hex(tt.a)
			if got != tt.want {
				t.Errorf("dec2Hex(%d) = %q, want %q", tt.a, got, tt.want)
			}
		})
	}
}
