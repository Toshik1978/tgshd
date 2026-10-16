package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/cmd/app"
	"github.com/Toshik1978/tgshd/pkg/command"
	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/modemprobe"
	hcfg "github.com/Toshik1978/tgshd/pkg/health/config"
	"github.com/Toshik1978/tgshd/pkg/health/runner"
	"github.com/Toshik1978/tgshd/pkg/sms"
	"github.com/Toshik1978/tgshd/pkg/sms/gammu"
)

const (
	// modemProbeCheckName is the modem probe check (its key is modem_probe/roundtrip).
	modemProbeCheckName = "modem_probe"
	// modemProbeStateKey is the kv key of the probe's persisted state.
	modemProbeStateKey = "modem_probe.state"
	// modemProbeInterval is how often the probe is advanced: it sends only in
	// its weekly slot, so this bounds how late a send or a timeout is noticed.
	modemProbeInterval = time.Minute
	// gammuInboxTimeout bounds one gammu inbox query, well inside the inbox
	// worker's 15 s tick, so an unreachable Postgres only fails that tick.
	gammuInboxTimeout = 10 * time.Second
)

// errProbeStateNotLoaded fails modem probe runs until its stored state has
// been restored, so a probe is never sent from (or judged against) an empty
// state that the stored one would then overwrite.
var errProbeStateNotLoaded = errors.New("modem probe state not loaded")

// gammuInboxProviders provides the incoming-SMS worker (BOT_GAMMU_DSN). It
// forwards the gammu inbox to Telegram whether or not health monitoring runs;
// with a modem probe configured, the probe's replies are consumed instead.
// The database is opened lazily: Postgres being down fails each tick, never
// the start.
func gammuInboxProviders() fx.Option {
	return fx.Provide(
		fx.Annotate(newGammuInbox, fx.As(new(sms.Inbox))),
		fx.Annotate(newInboxWorker, fx.As(new(app.Worker)), fx.ResultTags(`group:"worker"`)),
	)
}

// inboxParams are the dependencies of the inbox worker.
type inboxParams struct {
	fx.In

	Logger    *zap.Logger
	Publisher sms.Publisher
	Config    *config
	Inbox     sms.Inbox
	// Matcher is the modem probe; nil (or absent without health monitoring)
	// when no probe runs.
	Matcher sms.ProbeMatcher `optional:"true"`
}

// newInboxWorker builds the inbox worker, forwarding to the main chat.
func newInboxWorker(p inboxParams) *sms.InboxWorker {
	return sms.NewInboxWorker(p.Logger, p.Publisher, p.Config.ChatID, p.Inbox, p.Matcher, nil)
}

// newGammuInbox reads the inbox of the gammu database.
func newGammuInbox(logger *zap.Logger, db *sql.DB) gammuInbox {
	return gammuInbox{repo: gammu.NewRepository(logger, db)}
}

// inboxRepository is the inbox side of the gammu repository.
type inboxRepository interface {
	Unprocessed(ctx context.Context) ([]gammu.InboxMessage, error)
	MarkProcessed(ctx context.Context, ids []int64) error
}

// gammuInbox adapts the gammu repository to sms.Inbox, bounding each query
// by gammuInboxTimeout.
type gammuInbox struct {
	repo inboxRepository
}

// Unprocessed implements sms.Inbox.
func (i gammuInbox) Unprocessed(ctx context.Context) ([]sms.InboxMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, gammuInboxTimeout)
	defer cancel()

	rows, err := i.repo.Unprocessed(ctx)
	if err != nil {
		return nil, fmt.Errorf("gammu inbox: %w", err)
	}
	out := make([]sms.InboxMessage, len(rows))
	for k, r := range rows {
		out[k] = sms.InboxMessage{
			ID: r.ID, SenderNumber: r.Sender, TextDecoded: r.Text, UDH: r.UDH, ReceivingDateTime: r.ReceivedAt,
		}
	}

	return out, nil
}

// MarkProcessed implements sms.Inbox.
func (i gammuInbox) MarkProcessed(ctx context.Context, ids []int64) error {
	ctx, cancel := context.WithTimeout(ctx, gammuInboxTimeout)
	defer cancel()

	if err := i.repo.MarkProcessed(ctx, ids); err != nil {
		return fmt.Errorf("gammu inbox: %w", err)
	}

	return nil
}

// modemProbeParams are the dependencies of the modem probe check.
type modemProbeParams struct {
	fx.In

	Setup  *healthSetup
	Config *config
	// SMS is the gammu outbox, provided by the daemon with BOT_GAMMU_DSN. The
	// check --once graph never provides it, so there the probe is report-only:
	// it prints the stored probe state and never sends.
	SMS command.SmsSender `optional:"true"`
}

// modemProbeOut is the modem probe check and, for the inbox worker, the probe
// as its matcher (nil when no probe runs).
type modemProbeOut struct {
	fx.Out

	Set     checkSet `group:"health_checks"`
	Matcher sms.ProbeMatcher
}

// modemProbeChecks provides the modem probe when its section is present. It
// sends through the gammu outbox, so without BOT_GAMMU_DSN it is not run and
// a start error says why.
func modemProbeChecks(p modemProbeParams) modemProbeOut {
	if p.Setup.Config == nil || p.Setup.Config.ModemProbe == nil {
		return modemProbeOut{}
	}
	if p.Config.GammuDSN == "" {
		return modemProbeOut{Set: checkSet{Errors: []startError{{
			Key:  modemProbeCheckName + "/config/missing",
			Text: "modem_probe requires BOT_GAMMU_DSN; it is not run",
		}}}}
	}

	var sender modemprobe.Sender // nil: report-only
	if p.SMS != nil {
		sender = boundedSMS{sender: p.SMS}
	}
	set, matcher := buildModemProbeCheck(p.Setup.Config.ModemProbe, sender, os.LookupEnv, time.Now)
	if matcher == nil {
		return modemProbeOut{Set: set} // never a typed nil matcher
	}

	return modemProbeOut{Set: set, Matcher: matcher}
}

// buildModemProbeCheck builds the probe check; its state is persisted under
// modemProbeStateKey, it refuses to run until that was restored, and the
// weekly summary reads it. matcher
// is the probe for the inbox worker, nil when the section cannot be used.
func buildModemProbeCheck(
	mp *hcfg.ModemProbe, sender modemprobe.Sender, lookup modemprobe.LookupEnv, now func() time.Time,
) (set checkSet, matcher *probeState) {
	probe, err := modemprobe.New(*mp, sender, lookup, now)
	if err != nil {
		return checkSet{Errors: []startError{{Key: modemProbeCheckName + "/config/invalid", Text: err.Error()}}}, nil
	}
	st := &probeState{probe: probe}
	common := hcfg.Common{Interval: hcfg.Duration(modemProbeInterval), Mode: hcfg.ModeTLG, PauseInQuietWindows: true}

	return checkSet{
		Checks:     []health.Check{newCheck(modemProbeCheckName, common, st.run)},
		Persisters: []runner.Persister{runner.KVPersister{Key: modemProbeStateKey, State: st}},
		Probe:      st,
	}, st
}

// probeState is the probe's state as a runner.KVState and the inbox worker's
// sms.ProbeMatcher. It holds the probe's runs back, and defers messages from
// the modem's own number, until the stored state was restored: a reply polled
// earlier would otherwise be forwarded and marked processed, and the restored
// probe would then raise a false "did not come back".
type probeState struct {
	probe    *modemprobe.Probe
	restored atomic.Bool
}

// Defer implements sms.ProbeMatcher.
func (s *probeState) Defer(sender string) bool {
	return !s.restored.Load() && s.probe.FromModem(sender)
}

// Match implements sms.ProbeMatcher.
func (s *probeState) Match(sender, text string, at time.Time) bool {
	return s.probe.Match(sender, text, at)
}

// Export implements runner.KVState.
func (s *probeState) Export() (string, error) {
	data, err := s.probe.Export()
	if err != nil {
		return "", fmt.Errorf("modem probe: %w", err)
	}

	return string(data), nil
}

// Restore implements runner.KVState. The probe runs afterwards even when the
// stored state is corrupt: that is reported, and the probe starts afresh.
func (s *probeState) Restore(data string) error {
	defer s.restored.Store(true)

	if err := s.probe.Restore([]byte(data)); err != nil {
		return fmt.Errorf("modem probe: %w", err)
	}

	return nil
}

// Status returns the probe's state for the weekly summary.
func (s *probeState) Status() modemprobe.State {
	return s.probe.Status()
}

// run is the probe's Run, failing until Restore was called.
func (s *probeState) run(ctx context.Context) health.CheckResult {
	if !s.restored.Load() {
		return health.CheckResult{Err: errProbeStateNotLoaded}
	}

	return s.probe.Run(ctx)
}

// boundedSMS bounds each send through the gammu outbox by smsSendTimeout: the
// runner sends with a context that has no deadline.
type boundedSMS struct {
	sender command.SmsSender
}

// Publish implements modemprobe.Sender.
func (s boundedSMS) Publish(ctx context.Context, phone, msg string) error {
	ctx, cancel := context.WithTimeout(ctx, smsSendTimeout)
	defer cancel()

	if err := s.sender.Publish(ctx, phone, msg); err != nil {
		return fmt.Errorf("send sms: %w", err)
	}

	return nil
}
