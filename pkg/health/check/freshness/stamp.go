package freshness

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/httpcheck"
)

// kindUnreadable is the checker-error kind for a stamp that cannot be read.
const kindUnreadable = "unreadable"

// StampSource fetches a stamp over HTTP (a GET). A zero headerName sends no
// header. status is the HTTP status of the answer; err is set when there was
// none. The existing Source stays the file interface.
type StampSource interface {
	Fetch(ctx context.Context, stampURL, headerName, headerValue string) (status int, body []byte, err error)
}

// Option customises a Check.
type Option func(*Check)

// WithStampSource sets the HTTP source used by entries that configure a url.
func WithStampSource(s StampSource) Option {
	return func(c *Check) { c.stamps = s }
}

// location names where the stamp lives, for readings. A url is cut to its
// scheme and host: its userinfo, path or query may hold a secret, and
// readings are pushed to Telegram and email.
func (c *Check) location() string {
	if c.cfg.URL == "" {
		return c.cfg.Path
	}

	u, err := url.Parse(c.cfg.URL)
	if err != nil || u.Host == "" {
		return "the stamp endpoint"
	}

	return u.Scheme + "://" + u.Host
}

// httpStamp fetches and parses the stamp of a url entry, bounded by the
// entry's timeout. Every failure is a CheckError, so no problem opens or
// resolves on it.
func (c *Check) httpStamp(ctx context.Context) (time.Time, error) {
	value, err := httpcheck.HeaderSecret(c.cfg.Header, nil)
	if err != nil {
		return time.Time{}, c.unreadable(err)
	}

	var name string
	if c.cfg.Header != nil {
		name = c.cfg.Header.Name
	}

	if c.stamps == nil {
		return time.Time{}, c.unreadable(errors.New("no stamp source configured"))
	}

	bounded, cancel, _ := health.Bound(ctx, c.cfg.Timeout.Duration())
	defer cancel()

	status, body, err := c.stamps.Fetch(bounded, c.cfg.URL, name, value)
	if err != nil {
		// A *url.Error repeats the whole url; keep only its cause.
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}

		return time.Time{}, c.unreadable(fmt.Errorf("stamp endpoint did not answer: %w", err))
	}

	if status < 200 || status > 299 {
		return time.Time{}, c.unreadable(fmt.Errorf("stamp endpoint answered %d", status))
	}

	sec, ok := parseUnix(body)
	if !ok {
		return time.Time{}, c.unreadable(errors.New("stamp body is not a Unix time"))
	}

	return time.Unix(sec, 0), nil
}

func (c *Check) unreadable(err error) error {
	return health.CheckError{Subject: c.cfg.Name, Kind: kindUnreadable, Err: err}
}

// parseUnix reads Unix seconds: unsigned digits, optionally followed by a dot
// and a digits-only fraction, which is ignored.
func parseUnix(body []byte) (int64, bool) {
	whole, frac, hasFrac := strings.Cut(strings.TrimSpace(string(body)), ".")
	if !digits(whole) || (hasFrac && !digits(frac)) {
		return 0, false
	}
	sec, err := strconv.ParseInt(whole, 10, 64)

	return sec, err == nil
}

// digits reports whether s is one or more ASCII digits.
func digits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}
