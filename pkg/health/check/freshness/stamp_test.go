package freshness_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/freshness"
	"github.com/Toshik1978/tgshd/pkg/health/config"
)

type fakeStamp struct {
	status int
	body   string
	err    error

	calls                        int
	url, headerName, headerValue string
	deadline                     time.Time
	hasDeadline                  bool
}

func (f *fakeStamp) Fetch(ctx context.Context, stampURL, name, value string) (int, []byte, error) {
	f.calls++
	f.url, f.headerName, f.headerValue = stampURL, name, value
	f.deadline, f.hasDeadline = ctx.Deadline()

	return f.status, []byte(f.body), f.err
}

func urlEntry() config.Freshness {
	return config.Freshness{
		Name:            "db-dump",
		URL:             "https://example.org/stamp",
		MaxAge:          config.Duration(26 * time.Hour),
		FutureTolerance: config.Duration(time.Hour),
		Timeout:         config.Duration(5 * time.Second),
	}
}

func runStamp(cfg config.Freshness, src *fakeStamp, st freshness.State) health.CheckResult {
	c := freshness.New(cfg, &fakeSource{}, st, base, freshness.WithStampSource(src))

	return c.Run(context.Background())
}

func TestHTTPStamp(t *testing.T) {
	fresh := base().Add(-time.Hour).Unix()
	stale := base().Add(-30 * time.Hour).Unix()

	tests := []struct {
		name string
		body string
		want health.Verdict
	}{
		{"integer", strconv.FormatInt(fresh, 10), health.Good},
		{"trimmed", " " + strconv.FormatInt(fresh, 10) + "\n", health.Good},
		{"fraction ignored", strconv.FormatInt(fresh, 10) + ".987", health.Good},
		{"stale", strconv.FormatInt(stale, 10), health.Bad},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := freshness.NewKVState(nil)
			src := &fakeStamp{status: 200, body: tc.body}

			res := runStamp(urlEntry(), src, st)
			if res.Err != nil || len(res.Readings) != 1 {
				t.Fatalf("result = %+v", res)
			}
			if got := res.Readings[0].Verdict; got != tc.want {
				t.Fatalf("verdict = %v, want %v", got, tc.want)
			}
			if src.url != "https://example.org/stamp" {
				t.Fatalf("url = %q", src.url)
			}
			if _, present, ok := st.Stamp("db-dump"); !present || !ok {
				t.Fatalf("stamp not recorded: present=%v ok=%v", present, ok)
			}
		})
	}
}

func TestHTTPNoAnswerUnknown(t *testing.T) {
	tests := []struct {
		name    string
		src     *fakeStamp
		message string
	}{
		{"no answer", &fakeStamp{err: errors.New("connection refused")}, "stamp endpoint did not answer"},
		{"non-2xx", &fakeStamp{status: 503, body: "1"}, "answered 503"},
		{"redirect", &fakeStamp{status: 302}, "answered 302"},
		{"not a time", &fakeStamp{status: 200, body: "yesterday"}, "body is not a Unix time"},
		{"empty", &fakeStamp{status: 200}, "body is not a Unix time"},
		{"negative", &fakeStamp{status: 200, body: "-1700000000"}, "body is not a Unix time"},
		{"plus sign", &fakeStamp{status: 200, body: "+1700000000"}, "body is not a Unix time"},
		{"signed fraction", &fakeStamp{status: 200, body: "1700000000.-5"}, "body is not a Unix time"},
		{"letters in fraction", &fakeStamp{status: 200, body: "1700000000.5s"}, "body is not a Unix time"},
		{"empty fraction", &fakeStamp{status: 200, body: "1700000000."}, "body is not a Unix time"},
		{"no whole part", &fakeStamp{status: 200, body: ".5"}, "body is not a Unix time"},
		{"not found error", &fakeStamp{err: fmt.Errorf("open socket: %w", fs.ErrNotExist)}, "did not answer"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := freshness.NewKVState(nil)

			res := runStamp(urlEntry(), tc.src, st)
			if len(res.Readings) != 0 {
				t.Fatalf("readings = %+v, want none", res.Readings)
			}
			var ce health.CheckError
			if !errors.As(res.Err, &ce) {
				t.Fatalf("err = %v, want CheckError", res.Err)
			}
			if got := ce.Key("freshness"); got != "freshness/db-dump/unreadable" {
				t.Fatalf("key = %q", got)
			}
			if !strings.Contains(ce.Err.Error(), tc.message) {
				t.Fatalf("message = %q, want %q", ce.Err, tc.message)
			}
			if _, present, ok := st.Stamp("db-dump"); present || ok {
				t.Fatalf("present=%v ok=%v, want neither", present, ok)
			}
		})
	}
}

func TestStampHeaderSecret(t *testing.T) {
	header := func() *config.Header {
		return &config.Header{
			Name: "Authorization", ValueEnv: "TEST_STAMP_TOKEN", Prefix: "Bearer ",
			Placeholders: []string{"fill-me"},
		}
	}
	body := strconv.FormatInt(base().Add(-time.Hour).Unix(), 10)

	t.Run("sent", func(t *testing.T) {
		t.Setenv("TEST_STAMP_TOKEN", "s3cret-value")
		cfg := urlEntry()
		cfg.Header = header()
		src := &fakeStamp{status: 200, body: body}

		res := runStamp(cfg, src, freshness.NewKVState(nil))
		if res.Err != nil || len(res.Readings) != 1 {
			t.Fatalf("result = %+v", res)
		}
		if src.headerName != "Authorization" || src.headerValue != "Bearer s3cret-value" {
			t.Fatalf("header = %q: %q", src.headerName, src.headerValue)
		}
	})

	t.Run("no header", func(t *testing.T) {
		src := &fakeStamp{status: 200, body: body}
		runStamp(urlEntry(), src, freshness.NewKVState(nil))
		if src.headerName != "" || src.headerValue != "" {
			t.Fatalf("header = %q: %q", src.headerName, src.headerValue)
		}
	})

	for name, value := range map[string]string{"extra placeholder": "xx-fill-me-xx", "empty": " "} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TEST_STAMP_TOKEN", value)
			cfg := urlEntry()
			cfg.Header = header()
			src := &fakeStamp{status: 200, body: body}

			res := runStamp(cfg, src, freshness.NewKVState(nil))
			var ce health.CheckError
			if !errors.As(res.Err, &ce) || ce.Key("freshness") != "freshness/db-dump/unreadable" {
				t.Fatalf("err = %v", res.Err)
			}
			if src.calls != 0 {
				t.Fatalf("fetched %d times with an unusable secret", src.calls)
			}
			if strings.Contains(ce.Err.Error(), value) && strings.TrimSpace(value) != "" {
				t.Fatalf("error leaks the value: %v", ce.Err)
			}
			if len(res.Readings) != 0 {
				t.Fatalf("readings = %+v", res.Readings)
			}
		})
	}
}

// TestHTTPStampBoundedByEntryTimeout proves the fetch deadline follows the
// entry's timeout, for two different values.
func TestHTTPStampBoundedByEntryTimeout(t *testing.T) {
	body := strconv.FormatInt(base().Add(-time.Hour).Unix(), 10)
	for _, timeout := range []time.Duration{2 * time.Second, 7 * time.Second} {
		t.Run(timeout.String(), func(t *testing.T) {
			cfg := urlEntry()
			cfg.Timeout = config.Duration(timeout)
			src := &fakeStamp{status: 200, body: body}

			earliest := time.Now().Add(timeout)
			runStamp(cfg, src, freshness.NewKVState(nil))
			latest := time.Now().Add(timeout)
			if !src.hasDeadline || src.deadline.Before(earliest) || src.deadline.After(latest) {
				t.Fatalf("fetch deadline %v (set %v), want %s after the fetch started",
					src.deadline, src.hasDeadline, timeout)
			}
		})
	}
}

func TestHTTPStampWithoutSource(t *testing.T) {
	st := freshness.NewKVState(nil)
	c := freshness.New(urlEntry(), &fakeSource{}, st, base)

	res := c.Run(context.Background())
	var ce health.CheckError
	if !errors.As(res.Err, &ce) || ce.Key("freshness") != "freshness/db-dump/unreadable" ||
		!strings.Contains(ce.Err.Error(), "no stamp source configured") {
		t.Fatalf("err = %v, want an unreadable CheckError", res.Err)
	}
	if len(res.Readings) != 0 {
		t.Fatalf("readings = %+v", res.Readings)
	}
}

// TestHTTPStampContextEnded proves a run whose context ended keeps the error,
// so the runner sees the run failed instead of an empty success.
func TestHTTPStampContextEnded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := &fakeStamp{err: context.Canceled}
	c := freshness.New(urlEntry(), &fakeSource{}, freshness.NewKVState(nil), base, freshness.WithStampSource(src))

	res := c.Run(ctx)
	if !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("err = %v, want the context's error kept", res.Err)
	}
	if len(res.Readings) != 0 {
		t.Fatalf("readings = %+v", res.Readings)
	}
}

// TestHTTPStampNeverRepeatsURLSecret proves a secret in the stamp URL's query
// (or userinfo) appears neither in readings nor in checker errors, even when
// the transport error is a *url.Error carrying the whole URL.
func TestHTTPStampNeverRepeatsURLSecret(t *testing.T) {
	const secret = "SECRET-q-123"
	stampURL := "https://user:" + secret + "@example.org/stamp?key=" + secret

	fresh := strconv.FormatInt(base().Add(-time.Hour).Unix(), 10)
	stale := strconv.FormatInt(base().Add(-30*time.Hour).Unix(), 10)
	future := strconv.FormatInt(base().Add(5*time.Hour).Unix(), 10)

	tests := []struct {
		name string
		src  *fakeStamp
	}{
		{"fresh", &fakeStamp{status: 200, body: fresh}},
		{"stale", &fakeStamp{status: 200, body: stale}},
		{"future", &fakeStamp{status: 200, body: future}},
		{"transport error", &fakeStamp{err: fmt.Errorf("fetch stamp: %w",
			&url.Error{Op: "Get", URL: stampURL, Err: errors.New("connection refused")})}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := urlEntry()
			cfg.URL = stampURL

			res := runStamp(cfg, tc.src, freshness.NewKVState(nil))
			for _, r := range res.Readings {
				if strings.Contains(r.Text, secret) {
					t.Fatalf("reading leaks the secret: %q", r.Text)
				}
				if !strings.Contains(r.Text, "https://example.org") {
					t.Fatalf("reading %q does not name the stamp host", r.Text)
				}
			}
			if res.Err != nil && strings.Contains(res.Err.Error(), secret) {
				t.Fatalf("error leaks the secret: %v", res.Err)
			}
			if len(res.Readings) == 0 && res.Err == nil {
				t.Fatal("no reading and no error")
			}
		})
	}
}
