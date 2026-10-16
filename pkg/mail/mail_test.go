package mail_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/smtp"
	"strings"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/mail"
)

type fakeClient struct {
	startTLS  bool
	tlsCalled bool
	authed    bool
	from      string
	rcpts     []string
	data      bytes.Buffer
	failMail  error
	quit      bool
	fail      map[string]error
}

func (c *fakeClient) Extension(name string) (bool, string) {
	return name == "STARTTLS" && c.startTLS, ""
}

func (c *fakeClient) StartTLS(*tls.Config) error { c.tlsCalled = true; return c.fail["starttls"] }
func (c *fakeClient) Auth(smtp.Auth) error       { c.authed = true; return c.fail["auth"] }

func (c *fakeClient) Mail(from string) error {
	c.from = from
	return c.failMail
}

func (c *fakeClient) Rcpt(to string) error { c.rcpts = append(c.rcpts, to); return c.fail["rcpt"] }

func (c *fakeClient) Data() (io.WriteCloser, error) {
	if err := c.fail["data"]; err != nil {
		return nil, err
	}

	return &failWriter{c: c}, nil
}

func (c *fakeClient) Quit() error  { c.quit = true; return c.fail["quit"] }
func (c *fakeClient) Close() error { return nil }

type failWriter struct{ c *fakeClient }

func (w *failWriter) Write(p []byte) (int, error) {
	if err := w.c.fail["write"]; err != nil {
		return 0, err
	}

	return w.c.data.Write(p)
}

func (w *failWriter) Close() error { return w.c.fail["close"] }

type fakeDialer struct {
	client *fakeClient
	addr   string
	err    error
}

//nolint:ireturn // satisfies mail.Dialer.
func (d *fakeDialer) Dial(_ context.Context, addr string) (mail.Client, error) {
	d.addr = addr
	if d.err != nil {
		return nil, d.err
	}
	return d.client, nil
}

func fixedNow() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

func TestSend(t *testing.T) {
	tests := []struct {
		name      string
		cfg       mail.Config
		startTLS  bool
		wantTLS   bool
		wantAuth  bool
		wantRcpts []string
	}{
		{
			name:      "plain no auth",
			cfg:       mail.Config{Addr: "192.0.2.1:25", From: "bot@example.org", To: "ops@example.org"},
			wantRcpts: []string{"ops@example.org"},
		},
		{
			name: "starttls and auth, two recipients",
			cfg: mail.Config{
				Addr: "192.0.2.1:587",
				From: "Bot <bot@example.org>",
				To:   "a@example.org, b@example.org",
				User: "u",
				Pass: "secret",
			},
			startTLS:  true,
			wantTLS:   true,
			wantAuth:  true,
			wantRcpts: []string{"a@example.org", "b@example.org"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := &fakeClient{startTLS: tt.startTLS}
			d := &fakeDialer{client: fc}
			s := mail.NewSender(tt.cfg, d, fixedNow)
			if err := s.Send(context.Background(), "Проверка failed", "line1\nline2"); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if d.addr != tt.cfg.Addr {
				t.Errorf("addr = %q", d.addr)
			}
			if fc.tlsCalled != tt.wantTLS || fc.authed != tt.wantAuth {
				t.Errorf("tls=%v auth=%v", fc.tlsCalled, fc.authed)
			}
			if fc.from != "bot@example.org" {
				t.Errorf("envelope from = %q", fc.from)
			}
			if strings.Join(fc.rcpts, ",") != strings.Join(tt.wantRcpts, ",") {
				t.Errorf("rcpts = %v", fc.rcpts)
			}
			msg := fc.data.String()
			for _, want := range []string{
				"From: ", "To: ", "Date: Fri, 02 Jan 2026 03:04:05 +0000\r\n",
				"Subject: =?utf-8?q?", "Content-Type: text/plain; charset=utf-8\r\n",
				"\r\n\r\nline1\r\nline2",
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("message missing %q:\n%s", want, msg)
				}
			}
			if strings.Contains(msg, "secret") {
				t.Error("message leaks password")
			}
			if !fc.quit {
				t.Error("Quit not called")
			}
		})
	}
}

func TestSendErrors(t *testing.T) {
	good := mail.Config{Addr: "192.0.2.1:25", From: "bot@example.org", To: "ops@example.org"}
	t.Run("dial", func(t *testing.T) {
		e := errors.New("boom")
		err := mail.NewSender(good, &fakeDialer{err: e}, fixedNow).Send(context.Background(), "s", "b")
		if !errors.Is(err, e) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("mail", func(t *testing.T) {
		e := errors.New("rejected")
		d := &fakeDialer{client: &fakeClient{failMail: e}}
		err := mail.NewSender(good, d, fixedNow).Send(context.Background(), "s", "b")
		if !errors.Is(err, e) {
			t.Errorf("err = %v", err)
		}
	})
	for _, step := range []string{"starttls", "auth", "rcpt", "data", "write", "close"} {
		t.Run(step, func(t *testing.T) {
			e := errors.New(step + " failed")
			cfg := good
			cfg.User = "u"
			fc := &fakeClient{startTLS: true, fail: map[string]error{step: e}}
			err := mail.NewSender(cfg, &fakeDialer{client: fc}, fixedNow).Send(context.Background(), "s", "b")
			if !errors.Is(err, e) {
				t.Errorf("err = %v", err)
			}
		})
	}
	t.Run("quit failure ignored", func(t *testing.T) {
		fc := &fakeClient{fail: map[string]error{"quit": errors.New("quit failed")}}
		err := mail.NewSender(good, &fakeDialer{client: fc}, fixedNow).Send(context.Background(), "s", "b")
		if err != nil {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("bad address", func(t *testing.T) {
		cfg := good
		cfg.To = "not an address"
		err := mail.NewSender(cfg, &fakeDialer{client: &fakeClient{}}, fixedNow).Send(context.Background(), "s", "b")
		if err == nil {
			t.Error("want error")
		}
	})
}

func TestEnabled(t *testing.T) {
	if (mail.Config{}).Enabled() {
		t.Error("empty config enabled")
	}
	if !(mail.Config{Addr: "h:25"}).Enabled() {
		t.Error("config with addr not enabled")
	}
}

func TestSendSubjectInjection(t *testing.T) {
	fc := &fakeClient{}
	cfg := mail.Config{Addr: "192.0.2.1:25", From: "bot@example.org", To: "ops@example.org"}
	err := mail.NewSender(cfg, &fakeDialer{client: fc}, fixedNow).
		Send(context.Background(), "x\r\nBcc: evil@example.org", "b")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(fc.data.String(), "\r\n") {
		if strings.HasPrefix(line, "Bcc:") {
			t.Errorf("injected header: %q", line)
		}
	}
}
