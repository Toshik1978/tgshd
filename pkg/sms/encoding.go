package sms

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// ignoreChars is the list of ignored characters.
var ignoreChars = map[string]bool{
	"0009": true,
	"0000": true,
}

// encodeMessage encodes the string to GSM string.
func encodeMessage(msg string) string { //nolint:unused
	encoded := ""

	if msg == "" {
		return encoded
	}

	d := int64(0)
	for _, r := range msg {
		a := int64(r)

		if d != 0 {
			if 56320 <= a && a <= 57343 {
				codePoint := dec2Hex(65536 + ((d - 55296) << 10) + (a - 56320))
				encoded += codePoint
				d = 0
				continue
			} else {
				d = 0
			}
		}

		if 55296 <= a && a <= 56319 {
			d = a
		} else {
			cp := dec2Hex(a)
			for len(cp) < 4 {
				cp = "0" + cp
			}
			encoded += cp
		}
	}

	return encoded
}

// decodeMessage decodes GSM string to string.
func decodeMessage(encoded string) string {
	if encoded == "" {
		return ""
	}

	result := ""

	index := 0
	for index < len(encoded) {
		hexCode := encoded[index : index+4]
		index += 4
		if !ignoreChars[hexCode] {
			result += hex2Char(hexCode)
		}
	}

	return result
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
	return time.Date(int(year), time.Month(month), int(day), int(hour), int(minutes), int(sec), 0, time.Local)
}

func dec2Hex(a int64) string { //nolint:unused
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
