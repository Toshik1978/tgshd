package sms

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// Control characters skipped when decoding a GSM string (tab and null).
const (
	hexTab  = "0009"
	hexNull = "0000"

	hexDigits = "0123456789abcdef"
)

// encodeMessage encodes the string to a UCS2 hex string: one 4-hex code unit
// per UTF-16 code unit, so an astral character becomes its surrogate pair.
func encodeMessage(msg string) string {
	if msg == "" {
		return ""
	}

	var encoded strings.Builder
	for _, u := range utf16.Encode([]rune(msg)) {
		encoded.WriteByte(hexDigits[(u>>12)&0xF])
		encoded.WriteByte(hexDigits[(u>>8)&0xF])
		encoded.WriteByte(hexDigits[(u>>4)&0xF])
		encoded.WriteByte(hexDigits[u&0xF])
	}

	return encoded.String()
}

// decodeMessage decodes a UCS2 hex string back to a string, combining UTF-16
// surrogate pairs and skipping the tab and null control units.
func decodeMessage(encoded string) string {
	if encoded == "" {
		return ""
	}

	units := make([]uint16, 0, len(encoded)/4)
	for index := 0; index+4 <= len(encoded); index += 4 {
		hexCode := encoded[index : index+4]
		if hexCode == hexTab || hexCode == hexNull {
			continue
		}

		u, err := strconv.ParseUint(hexCode, 16, 16)
		if err != nil {
			continue
		}
		units = append(units, uint16(u))
	}

	return string(utf16.Decode(units))
}

// decodeDate convert SMS date to the Time structure. A malformed timestamp
// (fewer than six comma-separated fields) yields the zero time rather than
// panicking, so one bad message can't abort the whole SMS batch.
func decodeDate(d string) time.Time {
	fields := strings.Split(d, ",")
	if len(fields) < 6 {
		return time.Time{}
	}
	year, _ := strconv.ParseInt(fields[0], 10, 32)
	month, _ := strconv.ParseInt(fields[1], 10, 32)
	day, _ := strconv.ParseInt(fields[2], 10, 32)
	hour, _ := strconv.ParseInt(fields[3], 10, 32)
	minutes, _ := strconv.ParseInt(fields[4], 10, 32)
	sec, _ := strconv.ParseInt(fields[5], 10, 32)

	loc := time.Local //nolint:gosmopolitan // the modem reports timestamps in the server's local timezone

	return time.Date(int(year), time.Month(month), int(day), int(hour), int(minutes), int(sec), 0, loc)
}
