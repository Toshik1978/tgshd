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
	tests := []struct {
		name string
		msg  string
	}{
		{name: "ascii", msg: "Hello"},
		{name: "bmp non-ascii", msg: "Привет"},
		{name: "emoji (surrogate pair)", msg: "Hi 😀"},
		{name: "mixed astral and bmp", msg: "a😀b€c"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded := encodeMessage(tt.msg)
			decoded := decodeMessage(encoded)
			if decoded != tt.msg {
				t.Errorf("round trip: got %q, want %q", decoded, tt.msg)
			}
		})
	}
}

func TestDecodeMessageSurrogatePair(t *testing.T) {
	// U+1F600 GRINNING FACE encodes to the UTF-16 surrogate pair D83D DE00.
	got := decodeMessage("d83dde00")
	if got != "😀" {
		t.Errorf("decodeMessage(surrogate pair) = %q, want %q", got, "😀")
	}
}
