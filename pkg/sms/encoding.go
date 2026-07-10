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
)

// encodeMessage encodes the string to GSM string.
func encodeMessage(msg string) string {
	if msg == "" {
		return ""
	}

	d := int64(0)

	var encoded strings.Builder
	for _, r := range msg {
		a := int64(r)

		if d != 0 {
			if 56320 <= a && a <= 57343 {
				codePoint := dec2Hex(65536 + ((d - 55296) << 10) + (a - 56320))
				encoded.WriteString(codePoint)
				d = 0

				continue
			}

			d = 0
		}

		if 55296 <= a && a <= 56319 {
			d = a
		} else {
			cp := dec2Hex(a)
			for len(cp) < 4 {
				cp = "0" + cp
			}

			encoded.WriteString(cp)
		}
	}

	return encoded.String()
}

// decodeMessage decodes GSM string to string.
func decodeMessage(encoded string) string {
	if encoded == "" {
		return ""
	}

	var result strings.Builder
	for index := 0; index < len(encoded); index += 4 {
		hexCode := encoded[index : index+4]
		if hexCode != hexTab && hexCode != hexNull {
			result.WriteString(hex2Char(hexCode))
		}
	}

	return result.String()
}

// decodeDate convert SMS date to the Time structure.
func decodeDate(d string) time.Time {
	fields := strings.Split(d, ",")
	year, _ := strconv.ParseInt(fields[0], 10, 32)
	month, _ := strconv.ParseInt(fields[1], 10, 32)
	day, _ := strconv.ParseInt(fields[2], 10, 32)
	hour, _ := strconv.ParseInt(fields[3], 10, 32)
	minutes, _ := strconv.ParseInt(fields[4], 10, 32)
	sec, _ := strconv.ParseInt(fields[5], 10, 32)

	loc := time.Local //nolint:gosmopolitan // the modem reports timestamps in the server's local timezone

	return time.Date(int(year), time.Month(month), int(day), int(hour), int(minutes), int(sec), 0, loc)
}

func dec2Hex(a int64) string {
	return strconv.FormatInt(a, 16)
}

//nolint:gosec
func hex2Char(hex string) string {
	parsed, err := strconv.ParseInt(hex, 16, 32)
	if err != nil {
		return ""
	}
	char := int(parsed)

	if char <= 65535 {
		return string(rune(char))
	} else if char <= 1114111 {
		char -= 65536
		r1 := rune(55296 | (char >> 10))
		r2 := rune(56320 | (char & 1023))
		return strconv.Itoa(int(utf16.Encode([]rune{r1, r2})[0]))
	}

	return ""
}
