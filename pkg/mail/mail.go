// Package mail sends plain-text notification emails through an SMTP relay.
package mail

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// Config is the SMTP relay configuration.
type Config struct {
	Addr string `env:"BOT_SMTP_ADDR"`
	From string `env:"BOT_SMTP_FROM"`
	To   string `env:"BOT_SMTP_TO"`
	User string `env:"BOT_SMTP_USER"`
	Pass string `env:"BOT_SMTP_PASS"`
}

// Enabled reports whether an SMTP relay is configured.
func (c Config) Enabled() bool {
	return c.Addr != ""
}

// Client is the subset of *smtp.Client used by Sender.
type Client interface {
	Extension(name string) (bool, string)
	StartTLS(config *tls.Config) error
	Auth(a smtp.Auth) error
	Mail(from string) error
	Rcpt(to string) error
	Data() (io.WriteCloser, error)
	Quit() error
	Close() error
}

// Dialer opens an SMTP session to addr.
type Dialer interface {
	Dial(ctx context.Context, addr string) (Client, error)
}

// NetDialer is the real Dialer, built on net.Dialer and smtp.NewClient.
type NetDialer struct {
	Timeout time.Duration
}

// Dial connects to addr and reads the SMTP greeting.
//
//nolint:ireturn // Dialer contract returns the Client interface so tests can fake it.
func (d NetDialer) Dial(ctx context.Context, addr string) (Client, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("parse smtp address: %w", err)
	}
	nd := net.Dialer{Timeout: d.Timeout}
	conn, err := nd.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial smtp: %w", err)
	}

	dl, ok := ctx.Deadline()
	if !ok && d.Timeout > 0 {
		dl = time.Now().Add(d.Timeout)
		ok = true
	}

	if ok {
		_ = conn.SetDeadline(dl)
	}

	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		stop()
		_ = conn.Close()

		return nil, fmt.Errorf("smtp greeting: %w", err)
	}

	return &netClient{Client: c, stop: stop}, nil
}

// netClient stops the context watcher when the session is closed.
type netClient struct {
	*smtp.Client

	stop func() bool
}

// Close ends the session and stops the context watcher.
func (c *netClient) Close() error {
	c.stop()

	if err := c.Client.Close(); err != nil {
		return fmt.Errorf("close smtp session: %w", err)
	}

	return nil
}

// Sender delivers messages via SMTP.
type Sender struct {
	cfg    Config
	dialer Dialer
	now    func() time.Time
}

// NewSender creates a Sender. A nil dialer uses NetDialer; a nil now uses time.Now.
func NewSender(cfg Config, dialer Dialer, now func() time.Time) *Sender {
	if dialer == nil {
		dialer = NetDialer{Timeout: 30 * time.Second}
	}
	if now == nil {
		now = time.Now
	}

	return &Sender{cfg: cfg, dialer: dialer, now: now}
}

// Send delivers one plain-text email to the configured recipients.
func (s *Sender) Send(ctx context.Context, subject, body string) error {
	from, err := mail.ParseAddress(s.cfg.From)
	if err != nil {
		return fmt.Errorf("parse smtp from: %w", err)
	}
	tos, err := mail.ParseAddressList(s.cfg.To)
	if err != nil {
		return fmt.Errorf("parse smtp to: %w", err)
	}
	host, _, err := net.SplitHostPort(s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("parse smtp address: %w", err)
	}

	msg := s.build(from, tos, subject, body)

	c, err := s.dialer.Dial(ctx, s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("connect to smtp relay: %w", err)
	}
	defer func() { _ = c.Close() }()

	if err := s.handshake(c, host); err != nil {
		return err
	}

	return transmit(c, from.Address, tos, msg)
}

func transmit(c Client, from string, tos []*mail.Address, msg string) error {
	if err := c.Mail(from); err != nil {
		return fmt.Errorf("smtp MAIL: %w", err)
	}

	for _, to := range tos {
		if err := c.Rcpt(to.Address); err != nil {
			return fmt.Errorf("smtp RCPT %s: %w", to.Address, err)
		}
	}

	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}

	if _, err = io.WriteString(w, msg); err != nil {
		_ = w.Close()

		return fmt.Errorf("write smtp message: %w", err)
	}

	if err = w.Close(); err != nil {
		return fmt.Errorf("finish smtp message: %w", err)
	}

	// The message was accepted; a QUIT failure must not look like a send failure,
	// since a retry would duplicate the delivered mail.
	_ = c.Quit()

	return nil
}

func (s *Sender) handshake(c Client, host string) error {
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("smtp STARTTLS: %w", err)
		}
	}
	if s.cfg.User != "" {
		if err := c.Auth(smtp.PlainAuth("", s.cfg.User, s.cfg.Pass, host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}

	return nil
}

func (s *Sender) build(from *mail.Address, tos []*mail.Address, subject, body string) string {
	toList := make([]string, len(tos))
	for i, to := range tos {
		toList[i] = to.String()
	}
	body = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")

	var b strings.Builder
	b.WriteString("From: " + from.String() + "\r\n")
	b.WriteString("To: " + strings.Join(toList, ", ") + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", sanitizeHeader(subject)) + "\r\n")
	b.WriteString("Date: " + s.now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)

	return b.String()
}

// sanitizeHeader strips line breaks to prevent header injection.
func sanitizeHeader(v string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(v)
}
