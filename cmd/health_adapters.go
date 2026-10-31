package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	netmail "net/mail"
	"net/netip"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/command"
	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/network"
	"github.com/Toshik1978/tgshd/pkg/health/check/ups"
	hcfg "github.com/Toshik1978/tgshd/pkg/health/config"
	"github.com/Toshik1978/tgshd/pkg/health/notify"
	"github.com/Toshik1978/tgshd/pkg/host/lockfile"
	"github.com/Toshik1978/tgshd/pkg/host/sysfs"
	"github.com/Toshik1978/tgshd/pkg/mail"
	"github.com/Toshik1978/tgshd/pkg/ping"
	"github.com/Toshik1978/tgshd/pkg/power"
	"github.com/Toshik1978/tgshd/pkg/telegram/route"
)

const (
	// fallbackHostName prefixes health messages when the host name is unknown.
	fallbackHostName = "tgshd"
	// upsCheckName is the name of the UPS check (its keys are ups/<name>).
	upsCheckName = "ups"
	// smsSendTimeout bounds one gammu outbox insert: the runner bounds only
	// the sends of a tick (by its deadline), not a push made between ticks.
	smsSendTimeout = 30 * time.Second
)

// delivery holds the channels health messages go out on. sms and mailer are
// nil interfaces when not configured.
type delivery struct {
	telegram notify.TelegramSender
	sms      notify.Sender
	mailer   notify.Mailer
	errors   []startError
}

// newDelivery builds the Telegram sender (to the main chat), the SMS sender
// (gammu and BOT_SMS_PHONE both set) and the mailer (BOT_SMTP_ADDR set and
// valid; an invalid SMTP setup is a start error and email stays off).
func newDelivery(cfg *config, topics route.Topics, publisher command.Publisher, gammu command.SmsSender) delivery {
	d := delivery{telegram: telegramSender{publisher: publisher, chatID: cfg.ChatID, topics: topics}}
	if s := newSMSSender(gammu, cfg.SMSPhone); s != nil {
		d.sms = s
	}
	if cfg.SMTP.Enabled() {
		if err := validateSMTP(cfg.SMTP); err != nil {
			d.errors = append(d.errors, startError{"health/smtp/config", "email disabled: " + err.Error()})
		} else {
			d.mailer = mail.NewSender(cfg.SMTP, nil, nil)
		}
	}

	return d
}

// validateSMTP checks the relay settings: BOT_SMTP_FROM and BOT_SMTP_TO are
// required with BOT_SMTP_ADDR, and all three must parse.
func validateSMTP(c mail.Config) error {
	if !c.Enabled() {
		return nil
	}
	if _, _, err := net.SplitHostPort(c.Addr); err != nil {
		return fmt.Errorf("BOT_SMTP_ADDR must be host:port: %w", err)
	}
	if c.From == "" {
		return errors.New("BOT_SMTP_FROM is required with BOT_SMTP_ADDR")
	}
	if c.To == "" {
		return errors.New("BOT_SMTP_TO is required with BOT_SMTP_ADDR")
	}
	if _, err := netmail.ParseAddress(c.From); err != nil {
		return fmt.Errorf("BOT_SMTP_FROM: %w", err)
	}
	if _, err := netmail.ParseAddressList(c.To); err != nil {
		return fmt.Errorf("BOT_SMTP_TO: %w", err)
	}

	return nil
}

// telegramSender sends health messages to the main chat and its topics. The publisher's
// Telegram client carries its own timeout (telegramTimeout).
type telegramSender struct {
	publisher command.Publisher
	chatID    int64
	topics    route.Topics
}

// Send implements notify.TelegramSender: the route resolves to the main chat,
// its topic's thread and its silence.
func (s telegramSender) Send(ctx context.Context, r route.Route, text string) error {
	if err := s.publisher.Publish(ctx, s.topics.Resolve(s.chatID, r), text); err != nil {
		return fmt.Errorf("send health message to telegram: %w", err)
	}

	return nil
}

// smsSender sends health messages to the operator's phone through the gammu
// outbox, each send bounded by smsSendTimeout.
type smsSender struct {
	sender boundedSMS
	phone  string
}

// newSMSSender returns nil unless both the gammu backend and the phone are set.
func newSMSSender(sender command.SmsSender, phone string) *smsSender {
	if sender == nil || phone == "" {
		return nil
	}

	return &smsSender{sender: boundedSMS{sender: sender}, phone: phone}
}

// Send implements notify.Sender.
func (s *smsSender) Send(ctx context.Context, text string) error {
	return s.sender.Publish(ctx, s.phone, text)
}

// heartbeatFile is the heartbeat path as a runner.Heartbeat.
type heartbeatFile string

// WriteTick implements runner.Heartbeat.
func (h heartbeatFile) WriteTick(now time.Time) error {
	if err := lockfile.WriteTick(string(h), now); err != nil {
		return fmt.Errorf("write heartbeat %s: %w", string(h), err)
	}

	return nil
}

// maintenanceFile is the maintenance lock path as a runner.Maintenance.
type maintenanceFile string

// MaintenanceHeld implements runner.Maintenance.
func (m maintenanceFile) MaintenanceHeld() (bool, error) {
	held, err := lockfile.MaintenanceHeld(string(m))
	if err != nil {
		return false, fmt.Errorf("probe maintenance lock %s: %w", string(m), err)
	}

	return held, nil
}

// upsReader is the NUT status read of pkg/power.
type upsReader interface {
	Status(ctx context.Context) (power.Status, error)
}

// upsSource adapts pkg/power to the UPS check's source.
type upsSource struct {
	reader upsReader
}

// Status implements ups.Source.
func (s upsSource) Status(ctx context.Context) (ups.Status, error) {
	st, err := s.reader.Status(ctx)
	if err != nil {
		return ups.Status{}, fmt.Errorf("read ups status: %w", err)
	}

	return ups.Status{State: st.State, Charge: st.Charge, Voltage: st.Voltage}, nil
}

// networkChecks provides the network check when its section is present.
func networkChecks(hs *healthSetup, gateway *sysfs.Reader, logger *zap.Logger) checkSet {
	if hs.Config == nil || hs.Config.Network == nil {
		return checkSet{}
	}

	return buildNetworkCheck(hs.Config.Network, gatewaySource{gateway}, ping.New(logger))
}

// gatewaySource adapts *sysfs.Reader to network.GatewaySource: no usable
// default route is network.ErrNoRoute (the router down), while a route table
// that cannot be read stays a plain error (unreadable).
type gatewaySource struct {
	reader *sysfs.Reader
}

// DefaultGateway implements network.GatewaySource.
func (g gatewaySource) DefaultGateway() (netip.Addr, error) {
	gw, err := g.reader.DefaultGateway()
	if errors.Is(err, sysfs.ErrNoDefaultGateway) {
		return netip.Addr{}, fmt.Errorf("%w: %w", network.ErrNoRoute, err)
	}
	if err != nil {
		return netip.Addr{}, fmt.Errorf("read default gateway: %w", err)
	}

	return gw, nil
}

// buildNetworkCheck builds the network check from its section.
func buildNetworkCheck(n *hcfg.Network, gateway network.GatewaySource, pinger network.Pinger) checkSet {
	chk := network.New(gateway, pinger, n.InternetTargets)

	return checkSet{Checks: []health.Check{newCheck(network.Name, n.Common, chk.Run)}}
}

// upsChecks provides the UPS check when its section is present. It reads NUT
// through its own pkg/power instance, so a /power read in flight never makes
// the check's read fail.
func upsChecks(hs *healthSetup, cfg *config, logger *zap.Logger) checkSet {
	if hs.Config == nil || hs.Config.UPS == nil {
		return checkSet{}
	}
	p := power.New(logger, cfg.NutIP, cfg.NutName, cfg.NutUser, cfg.NutPassword)

	return buildUPSCheck(hs.Config.UPS, p.Valid(), p, cfg.NutName)
}

// buildUPSCheck builds the UPS check; without a complete BOT_NUT_* setup it
// is not run and a start error says why.
func buildUPSCheck(u *hcfg.UPS, configured bool, reader upsReader, name string) checkSet {
	if !configured {
		return checkSet{Errors: []startError{{
			Key:  upsCheckName + "/config/missing",
			Text: "the ups check needs BOT_NUT_IP, BOT_NUT_NAME, BOT_NUT_USER and BOT_NUT_PASS; it is not run",
		}}}
	}
	chk := ups.New(upsSource{reader: reader}, name)

	return checkSet{Checks: []health.Check{newCheck(upsCheckName, u.Common, chk.Run)}}
}
