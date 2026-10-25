package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	dockerclient "github.com/moby/moby/client"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/memory"
	"github.com/Toshik1978/tgshd/pkg/health/check/network"
	hcfg "github.com/Toshik1978/tgshd/pkg/health/config"
	"github.com/Toshik1978/tgshd/pkg/health/memguard"
	"github.com/Toshik1978/tgshd/pkg/health/notify"
	"github.com/Toshik1978/tgshd/pkg/health/runner"
	"github.com/Toshik1978/tgshd/pkg/health/store"
	"github.com/Toshik1978/tgshd/pkg/host/cgroupmem"
	"github.com/Toshik1978/tgshd/pkg/host/lockfile"
	"github.com/Toshik1978/tgshd/pkg/host/procmem"
	"github.com/Toshik1978/tgshd/pkg/host/sysfs"
	"github.com/Toshik1978/tgshd/pkg/mail"
	"github.com/Toshik1978/tgshd/pkg/power"
	"github.com/Toshik1978/tgshd/pkg/telegram"
)

// fakePublisher records Telegram messages; it is safe for concurrent use.
type fakePublisher struct {
	mu   sync.Mutex
	err  error
	sent []string
	to   []int64
}

func (f *fakePublisher) Publish(_ context.Context, recipientID int64, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.to = append(f.to, recipientID)
	f.sent = append(f.sent, msg)

	return f.err
}

func (f *fakePublisher) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.sent)
}

// fakeSMS records outgoing SMS.
type fakeSMS struct {
	err         error
	phone, msg  string
	hadDeadline bool
}

func (f *fakeSMS) Publish(ctx context.Context, phone, msg string) error {
	f.phone, f.msg = phone, msg
	_, f.hadDeadline = ctx.Deadline()

	return f.err
}

type fakeGateway struct{}

func (fakeGateway) DefaultGateway() (netip.Addr, error) {
	return netip.MustParseAddr("192.0.2.1"), nil
}

type fakePinger struct{ up map[string]bool }

func (f fakePinger) Reachable(_ context.Context, _ []string) (map[string]bool, error) {
	return f.up, nil
}

func TestHostName(t *testing.T) {
	t.Parallel()

	ok := func() (string, error) { return "box.example.org", nil }
	fail := func() (string, error) { return "", errors.New("no hostname") }
	empty := func() (string, error) { return "", nil }

	tests := []struct {
		name     string
		override string
		hostname func() (string, error)
		want     string
	}{
		{"override wins", "web-1", ok, "web-1"},
		{"os hostname", "", ok, "box.example.org"},
		{"hostname error", "", fail, fallbackHostName},
		{"hostname empty", "", empty, fallbackHostName},
		{"blank override", "  ", ok, "box.example.org"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := hostName(tt.override, tt.hostname); got != tt.want {
				t.Fatalf("hostName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateSMTP(t *testing.T) {
	t.Parallel()

	valid := mail.Config{Addr: "smtp.example.org:587", From: "tgshd@example.org", To: "ops@example.org, b@example.org"}
	tests := []struct {
		name    string
		mutate  func(*mail.Config)
		wantErr string
	}{
		{"disabled", func(c *mail.Config) { *c = mail.Config{} }, ""},
		{"valid", func(*mail.Config) {}, ""},
		{"missing from", func(c *mail.Config) { c.From = "" }, "BOT_SMTP_FROM is required"},
		{"missing to", func(c *mail.Config) { c.To = "" }, "BOT_SMTP_TO is required"},
		{"no port", func(c *mail.Config) { c.Addr = "smtp.example.org" }, "BOT_SMTP_ADDR"},
		{"bad from", func(c *mail.Config) { c.From = "not an address" }, "BOT_SMTP_FROM"},
		{"bad to", func(c *mail.Config) { c.To = "ops@example.org, not an address" }, "BOT_SMTP_TO"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := valid
			tt.mutate(&c)
			err := validateSMTP(c)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateSMTP() = %v, want nil", err)
				}

				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateSMTP() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestWeeklySlot(t *testing.T) {
	t.Parallel()

	core, logs := observer.New(zap.WarnLevel)
	logger := zap.New(core)

	if got := weeklySlot(nil, logger); got != nil {
		t.Fatalf("weeklySlot(nil) = %+v, want nil", got)
	}

	got := weeklySlot(&hcfg.Summary{WeekdayName: "tuesday", Time: "09:30", Weekday: time.Tuesday}, logger)
	want := runner.Slot{Weekday: time.Tuesday, Hour: 9, Minute: 30}
	if got == nil || *got != want {
		t.Fatalf("weeklySlot() = %+v, want %+v", got, want)
	}
	if logs.Len() != 0 {
		t.Fatalf("valid slots must not log: %v", logs.All())
	}

	if got := weeklySlot(&hcfg.Summary{Time: "bogus"}, logger); got != nil {
		t.Fatalf("weeklySlot(bad time) = %+v, want nil", got)
	}
	if logs.Len() != 1 || !strings.Contains(logs.All()[0].Message, "summary") {
		t.Fatalf("a bad summary time must be logged: %v", logs.All())
	}
}

func TestNewCheck(t *testing.T) {
	t.Parallel()

	common := hcfg.Common{
		Interval:            hcfg.Duration(5 * time.Minute),
		Threshold:           hcfg.Duration(time.Minute),
		Mode:                hcfg.ModeTLGOrSMS,
		NeedsInternet:       true,
		PauseInQuietWindows: true,
	}
	ran := false
	chk := newCheck("ups", common, func(context.Context) health.CheckResult {
		ran = true

		return health.CheckResult{}
	})

	if chk.Name != "ups" || chk.Interval != 5*time.Minute || chk.Threshold != time.Minute ||
		chk.Mode != health.ModeTelegramOrSMS || !chk.NeedsInternet || !chk.PauseInQuiet || chk.DependsOn != "" {
		t.Fatalf("newCheck() = %+v", chk)
	}
	chk.Run(t.Context())
	if !ran {
		t.Fatal("Run did not call the check")
	}
}

func TestConfigErrors(t *testing.T) {
	t.Parallel()

	got := configErrors([]hcfg.EntryError{
		{File: "/etc/tgshd/health.yaml", Reason: "read: no such file"},
		{File: "/etc/tgshd/health.yaml", Entry: "http/web-app", Reason: "line 3: bad url"},
	})
	want := []startError{
		{Key: "config/file/unreadable", Text: "/etc/tgshd/health.yaml: read: no such file"},
		{Key: "config/http/web-app/invalid", Text: "/etc/tgshd/health.yaml: http/web-app: line 3: bad url"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("configErrors() = %+v, want %+v", got, want)
	}
}

func TestMergeCheckSets(t *testing.T) {
	t.Parallel()

	book := &fakeDriveBook{}
	p1, p2 := &fakePersister{}, &fakePersister{}
	set := mergeCheckSets([]checkSet{
		{
			Checks: []health.Check{{Name: "ups"}}, Errors: []startError{{Key: "ups/a", Text: "a"}},
			Persisters: []runner.Persister{p1},
		},
		{},
		{
			Checks: []health.Check{{Name: "network"}}, Persisters: []runner.Persister{p2}, Drives: book,
			Background: []background{{Key: "kernel/kmsg/read"}},
		},
	})
	if len(set.Checks) != 2 || set.Checks[0].Name != "network" || set.Checks[1].Name != "ups" {
		t.Fatalf("checks = %+v, want network then ups", set.Checks)
	}
	if len(set.Errors) != 1 || set.Errors[0].Key != "ups/a" {
		t.Fatalf("errors = %+v", set.Errors)
	}
	if len(set.Persisters) != 2 || set.Persisters[0] != p1 || set.Persisters[1] != p2 {
		t.Fatalf("persisters = %+v", set.Persisters)
	}
	if set.Drives != book || len(set.Background) != 1 || set.Background[0].Key != "kernel/kmsg/read" {
		t.Fatalf("drives = %v, background = %+v", set.Drives, set.Background)
	}
	if empty := mergeCheckSets(nil); empty.Drives != nil {
		t.Fatalf("no disks check: drives = %v, want a nil interface", empty.Drives)
	}
}

func TestBuildNetworkCheck(t *testing.T) {
	t.Parallel()

	n := &hcfg.Network{
		Interval: hcfg.Duration(time.Minute), Mode: hcfg.ModeSMS,
		InternetTargets: []string{"198.51.100.20"},
	}
	set := buildNetworkCheck(n, fakeGateway{}, fakePinger{up: map[string]bool{"192.0.2.1": true}})
	if len(set.Checks) != 1 || len(set.Errors) != 0 {
		t.Fatalf("set = %+v", set)
	}
	chk := set.Checks[0]
	if chk.Name != notify.CheckNetwork || chk.Mode != health.ModeSMS || chk.Interval != time.Minute {
		t.Fatalf("check = %+v", chk)
	}
	res := chk.Run(t.Context())
	if len(res.Readings) != 1 || res.Readings[0].Key != network.KeyConnectivity ||
		res.Readings[0].Verdict != health.Bad {
		t.Fatalf("readings = %+v", res.Readings)
	}
}

// TestGatewaySourceMapsNoRoute: the sysfs reader's "no default gateway" is
// the pure network.ErrNoRoute (judged as the router down), while a route
// table that cannot be read stays a plain error (unreadable).
func TestGatewaySourceMapsNoRoute(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const header = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"
	writeRoute := func(name, line string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(header+line), 0o600); err != nil {
			t.Fatal(err)
		}

		return path
	}
	noDefault := writeRoute("route-no-default", "eth0\t0002000A\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n")
	withDefault := writeRoute("route-default", "eth0\t00000000\t010200C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n")

	gw, err := gatewaySource{sysfs.New("", withDefault)}.DefaultGateway()
	if err != nil || gw != netip.MustParseAddr("192.0.2.1") {
		t.Fatalf("default route: %v, %v", gw, err)
	}
	if _, err = (gatewaySource{sysfs.New("", noDefault)}).DefaultGateway(); !errors.Is(err, network.ErrNoRoute) {
		t.Fatalf("no default route: err = %v, want network.ErrNoRoute", err)
	}
	_, err = gatewaySource{sysfs.New("", filepath.Join(dir, "missing"))}.DefaultGateway()
	if err == nil || errors.Is(err, network.ErrNoRoute) {
		t.Fatalf("missing route file: err = %v, want a plain error", err)
	}
}

func TestCheckProvidersFollowTheConfig(t *testing.T) {
	t.Parallel()

	logger := zap.NewNop()
	reader := sysfs.NewDefault()
	cfg := &config{}

	for _, hs := range []*healthSetup{{}, {Config: &hcfg.Config{}}} {
		if set := networkChecks(hs, reader, logger); len(set.Checks)+len(set.Errors) != 0 {
			t.Fatalf("network without a section: %+v", set)
		}
		if set := upsChecks(hs, cfg, logger); len(set.Checks)+len(set.Errors) != 0 {
			t.Fatalf("ups without a section: %+v", set)
		}
	}

	hs := &healthSetup{Config: &hcfg.Config{
		Network: &hcfg.Network{InternetTargets: []string{"198.51.100.20"}},
		UPS:     &hcfg.UPS{},
	}}
	if set := networkChecks(hs, reader, logger); len(set.Checks) != 1 {
		t.Fatalf("network section: %+v", set)
	}
	if set := upsChecks(hs, cfg, logger); len(set.Checks) != 0 || len(set.Errors) != 1 {
		t.Fatalf("ups section without BOT_NUT_*: %+v", set)
	}
	cfg = &config{NutIP: "192.0.2.10", NutName: "ups", NutUser: "monitor", NutPassword: "secret"}
	if set := upsChecks(hs, cfg, logger); len(set.Checks) != 1 || len(set.Errors) != 0 {
		t.Fatalf("ups section with BOT_NUT_*: %+v", set)
	}
}

type fakeUPSStatus struct {
	st  power.Status
	err error
}

func (f fakeUPSStatus) Status(context.Context) (power.Status, error) { return f.st, f.err }

func TestBuildUPSCheck(t *testing.T) {
	t.Parallel()

	u := &hcfg.UPS{Interval: hcfg.Duration(time.Minute), Mode: hcfg.ModeTLG}

	set := buildUPSCheck(u, false, fakeUPSStatus{}, "ups")
	if len(set.Checks) != 0 || len(set.Errors) != 1 || set.Errors[0].Key != "ups/config/missing" {
		t.Fatalf("unconfigured NUT: set = %+v", set)
	}

	charge := 97.0
	set = buildUPSCheck(u, true, fakeUPSStatus{st: power.Status{State: "OL", Charge: &charge}}, "rack")
	if len(set.Checks) != 1 || len(set.Errors) != 0 || set.Checks[0].Name != upsCheckName {
		t.Fatalf("configured NUT: set = %+v", set)
	}
	res := set.Checks[0].Run(t.Context())
	if len(res.Readings) != 1 || res.Readings[0].Key != "ups/rack" || res.Readings[0].Verdict != health.Good {
		t.Fatalf("readings = %+v", res.Readings)
	}
	if len(res.Samples) != 1 || res.Samples[0].Value != charge {
		t.Fatalf("samples = %+v", res.Samples)
	}
}

func TestUPSSource(t *testing.T) {
	t.Parallel()

	v := 230.5
	src := upsSource{reader: fakeUPSStatus{st: power.Status{State: "OB", Voltage: &v}}}
	st, err := src.Status(t.Context())
	if err != nil || st.State != "OB" || st.Voltage == nil || *st.Voltage != v || st.Charge != nil {
		t.Fatalf("Status() = %+v, %v", st, err)
	}

	src = upsSource{reader: fakeUPSStatus{err: power.ErrStale}}
	if _, err := src.Status(t.Context()); !errors.Is(err, power.ErrStale) {
		t.Fatalf("Status() error = %v, want ErrStale", err)
	}
}

func TestTelegramSender(t *testing.T) {
	t.Parallel()

	pub := &fakePublisher{}
	s := telegramSender{publisher: pub, chatID: 42}
	if err := s.Send(t.Context(), "hello"); err != nil {
		t.Fatalf("Send() = %v", err)
	}
	if pub.to[0] != 42 || pub.sent[0] != "hello" {
		t.Fatalf("published %v to %v", pub.sent, pub.to)
	}

	pub.err = errors.New("boom")
	if err := s.Send(t.Context(), "x"); !errors.Is(err, pub.err) {
		t.Fatalf("Send() = %v, want wrapped boom", err)
	}
}

func TestSMSSender(t *testing.T) {
	t.Parallel()

	if s := newSMSSender(nil, "+10000000000"); s != nil {
		t.Fatal("no gammu backend must give no sender")
	}
	if s := newSMSSender(&fakeSMS{}, ""); s != nil {
		t.Fatal("no phone must give no sender")
	}

	backend := &fakeSMS{}
	s := newSMSSender(backend, "+10000000000")
	if err := s.Send(context.Background(), "down"); err != nil {
		t.Fatalf("Send() = %v", err)
	}
	if backend.phone != "+10000000000" || backend.msg != "down" || !backend.hadDeadline {
		t.Fatalf("backend got %+v; the send must be bounded by its own deadline", backend)
	}

	backend.err = errors.New("db down")
	if err := s.Send(t.Context(), "x"); !errors.Is(err, backend.err) {
		t.Fatalf("Send() = %v, want wrapped db down", err)
	}
}

func TestFileAdapters(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	hb := heartbeatFile(filepath.Join(dir, "run", "heartbeat"))
	now := time.Unix(1_700_000_000, 0)
	if err := hb.WriteTick(now); err != nil {
		t.Fatalf("WriteTick() = %v", err)
	}
	got, err := lockfile.ReadHeartbeat(string(hb))
	if err != nil || got.TS != now.Unix() {
		t.Fatalf("heartbeat = %+v, %v", got, err)
	}

	held, err := maintenanceFile(filepath.Join(dir, "missing.lock")).MaintenanceHeld()
	if err != nil || held {
		t.Fatalf("MaintenanceHeld() = %v, %v; want false, nil", held, err)
	}
}

func TestNewDelivery(t *testing.T) {
	t.Parallel()

	cfg := &config{ChatID: 7, SMSPhone: "+10000000000"}
	d := newDelivery(cfg, &fakePublisher{}, nil)
	if d.telegram == nil || d.sms != nil || d.mailer != nil || len(d.errors) != 0 {
		t.Fatalf("no gammu, no smtp: %+v", d)
	}

	d = newDelivery(cfg, &fakePublisher{}, &fakeSMS{})
	if d.sms == nil {
		t.Fatal("gammu and phone set: want an SMS sender")
	}

	cfg.SMTP = mail.Config{Addr: "smtp.example.org:25", From: "tgshd@example.org"}
	d = newDelivery(cfg, &fakePublisher{}, nil)
	if d.mailer != nil || len(d.errors) != 1 || d.errors[0].Key != "health/smtp/config" {
		t.Fatalf("invalid smtp: %+v", d)
	}

	cfg.SMTP.To = "ops@example.org"
	d = newDelivery(cfg, &fakePublisher{}, nil)
	if d.mailer == nil || len(d.errors) != 0 {
		t.Fatalf("valid smtp: %+v", d)
	}
}

func TestRunnerOptions(t *testing.T) {
	t.Parallel()

	hc := &hcfg.Config{
		General: hcfg.General{
			HeartbeatFile: "/run/tgshd/heartbeat", BootGrace: hcfg.Duration(time.Minute), Concurrency: 3,
		},
		Summary: &hcfg.Summary{Weekday: time.Monday, Time: "09:00"},
	}
	hs := &healthSetup{Config: hc, Host: "box"}
	d := delivery{telegram: telegramSender{}}
	checks := []health.Check{{Name: "network"}}

	opts := runnerOptions(hs, d, checkSet{Checks: checks}, sysfs.NewDefault(), zap.NewNop())
	if opts.Store != nil || opts.Maintenance != nil || opts.SMS != nil || opts.Mailer != nil ||
		opts.Drives != nil || len(opts.Persisters) != 0 {
		t.Fatalf("absent facilities must be nil interfaces: %+v", opts)
	}
	if opts.Heartbeat == nil || opts.Uptime == nil || opts.Telegram == nil || opts.Host != "box" ||
		opts.BootGrace != time.Minute || opts.Concurrency != 3 || len(opts.Checks) != 1 ||
		opts.Mode != notify.ModeTelegram {
		t.Fatalf("opts = %+v", opts)
	}
	if opts.WeeklySlot == nil || opts.WeeklySlot.Weekday != time.Monday || opts.WeeklySlot.Hour != 9 {
		t.Fatalf("weekly slot = %+v", opts.WeeklySlot)
	}

	hc.General.MaintenanceLock = "/run/maintenance.lock"
	hc.General.MaintenanceMaxHold = hcfg.Duration(2 * time.Hour)
	hc.General.TelegramFailingAfter = hcfg.Duration(30 * time.Minute)
	opts = runnerOptions(hs, d, checkSet{Checks: checks}, sysfs.NewDefault(), zap.NewNop())
	if opts.Maintenance == nil {
		t.Fatal("maintenance lock configured: want a probe")
	}
	if opts.MaintenanceMaxHold != 2*time.Hour || opts.TelegramFailingAfter != 30*time.Minute {
		t.Fatalf("maintenance max hold = %v, telegram failing after = %v; want 2h and 30m from general",
			opts.MaintenanceMaxHold, opts.TelegramFailingAfter)
	}

	sms, mailer := &recordingSender{}, &fakeMailer{}
	opts = runnerOptions(hs, delivery{telegram: telegramSender{}, sms: sms, mailer: mailer},
		checkSet{Checks: checks}, sysfs.NewDefault(), zap.NewNop())
	if opts.SMS != sms || opts.Mailer != mailer {
		t.Fatalf("sms = %v, mailer = %v; want the configured SMS fallback sender and mailer", opts.SMS, opts.Mailer)
	}

	book, p, fixes := &fakeDriveBook{}, &fakePersister{}, &fakeFixSource{}
	set := checkSet{
		Checks: checks, Drives: book, Persisters: []runner.Persister{p}, Fixes: []runner.FixSource{fixes},
	}
	opts = runnerOptions(hs, d, set, sysfs.NewDefault(), zap.NewNop())
	if opts.Drives != book || len(opts.Persisters) != 1 || opts.Persisters[0] != p {
		t.Fatalf("drives = %v, persisters = %v; want the disks book and the persisters", opts.Drives, opts.Persisters)
	}
	if len(opts.Fixes) != 1 || opts.Fixes[0] != fixes {
		t.Fatalf("fixes = %v, want the command checks' fix sources", opts.Fixes)
	}
}

func TestRegisterStartLoadsBeforeStartReturnsThenRaises(t *testing.T) {
	t.Parallel()

	lc := fxtest.NewLifecycle(t)
	var (
		mu     sync.Mutex
		events []string
	)
	record := func(e string) {
		mu.Lock()
		defer mu.Unlock()

		events = append(events, e)
	}
	load := func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("load must run with a deadline")
		}
		record("load")

		return nil
	}
	raise := func(key, _ string) { record("raise " + key) }
	registerStart(lc, load, startLoadTimeout, raise, []startError{{Key: "a"}, {Key: "b"}})

	lc.RequireStart()
	mu.Lock()
	if len(events) == 0 || events[0] != "load" {
		t.Errorf("OnStart returned before the state was loaded: %v", events)
	}
	mu.Unlock()
	lc.RequireStop()

	if want := []string{"load", "raise a", "raise b"}; !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRegisterStartWithoutLoad(t *testing.T) {
	t.Parallel()

	lc := fxtest.NewLifecycle(t)
	var raised []string
	registerStart(lc, nil, startLoadTimeout, func(key, _ string) { raised = append(raised, key) },
		[]startError{{Key: "config/file/unreadable"}})
	lc.RequireStart().RequireStop()

	if !slices.Equal(raised, []string{"config/file/unreadable"}) {
		t.Fatalf("raised = %v", raised)
	}
}

// blockingStore is a runner store whose Load blocks until its context ends.
type blockingStore struct{}

func (blockingStore) Load(ctx context.Context) (store.State, error) {
	<-ctx.Done()

	return store.State{}, fmt.Errorf("read state: %w", ctx.Err())
}

func (blockingStore) Checkpoint(context.Context, store.State, []store.Alert, []store.SampleRow) error {
	return nil
}

func (blockingStore) Degraded() bool { return false }

func TestRegisterStartSlowStoreStillStartsAndFirstTickRaises(t *testing.T) {
	t.Parallel()

	pub := &fakePublisher{}
	r := runner.New(runner.Options{
		Store: blockingStore{}, Telegram: telegramSender{publisher: pub}, Host: "box", Logger: zap.NewNop(),
	})
	lc := fxtest.NewLifecycle(t)
	registerStart(lc, r.Load, 50*time.Millisecond, r.CheckerError, nil)
	next := false
	lc.Append(fx.Hook{OnStart: func(context.Context) error {
		next = true

		return nil
	}})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := lc.Start(ctx); err != nil {
		t.Fatalf("Start() = %v; a slow store must not fail start", err)
	}
	if !next {
		t.Fatal("the hooks after the health start hook must run")
	}
	if got := pub.messages(); len(got) != 0 {
		t.Fatalf("start sent %q, want the load failure kept for the first tick", got)
	}

	tickCtx, tickCancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer tickCancel()
	_ = r.Do(tickCtx) // The tick's own Load times out on the blocking store too.

	got := pub.messages()
	if len(got) != 1 || !strings.HasPrefix(got[0], "box: CHECKER ERROR: health: cannot load state: ") {
		t.Fatalf("messages = %q, want the load failure raised by the first tick", got)
	}
	lc.RequireStop()
}

func writeHealthConfig(t *testing.T, body string) (path, stateDir string) {
	t.Helper()

	dir := t.TempDir()
	stateDir = filepath.Join(dir, "state")
	path = filepath.Join(dir, "health.yaml")
	content := "general:\n  state_dir: " + stateDir + "\n  heartbeat_file: " +
		filepath.Join(dir, "heartbeat") + "\n" + body
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	return path, stateDir
}

func TestNewHealthSetupClaimsStateAndLocksOutASecondInstance(t *testing.T) {
	t.Parallel()

	path, stateDir := writeHealthConfig(t, "ups: {}\nbogus: {}\n")
	cfg := &config{HealthConfig: path, HostName: "box"}

	lc := fxtest.NewLifecycle(t)
	hs := newHealthSetup(lc, zap.NewNop(), cfg)
	if hs.Config == nil || hs.Store == nil || hs.Host != "box" {
		t.Fatalf("first instance: %+v", hs)
	}
	if len(hs.Errors) != 1 || hs.Errors[0].Key != "config/bogus/invalid" {
		t.Fatalf("entry errors = %+v", hs.Errors)
	}
	info, err := os.Stat(stateDir)
	if err != nil || info.Mode().Perm() != stateDirMode {
		t.Fatalf("state dir: %v, %v", info, err)
	}

	second := newHealthSetup(fxtest.NewLifecycle(t), zap.NewNop(), cfg)
	if second.Config != nil || second.Store != nil || len(second.Errors) != 0 {
		t.Fatalf("second instance must run without health and raise nothing: %+v", second)
	}

	lc.RequireStart().RequireStop()
	thirdLC := fxtest.NewLifecycle(t)
	third := newHealthSetup(thirdLC, zap.NewNop(), cfg)
	if third.Config == nil || third.Store == nil {
		t.Fatal("the lock must be released on stop")
	}
	// Stopping releases the third setup's store and lock fd.
	thirdLC.RequireStart().RequireStop()
}

// TestNewHealthSetupRetriesAFailedStoreOpen: a state database that cannot be
// opened at start is opened by a later use (the runner's Load each tick), so
// the daemon does not run from memory until it restarts.
func TestNewHealthSetupRetriesAFailedStoreOpen(t *testing.T) {
	t.Parallel()

	path, stateDir := writeHealthConfig(t, "")
	// A directory where the database belongs: the open fails until it is gone.
	dbPath := filepath.Join(stateDir, store.FileName)
	if err := os.MkdirAll(dbPath, 0o700); err != nil {
		t.Fatal(err)
	}

	lc := fxtest.NewLifecycle(t)
	hs := newHealthSetup(lc, zap.NewNop(), &config{HealthConfig: path, HostName: "box"})
	if hs.Config == nil || hs.Store == nil {
		t.Fatalf("setup = %+v, want monitoring with a store to retry", hs)
	}
	if !hs.Store.Degraded() {
		t.Fatal("a store that is not open must read as degraded")
	}
	if _, err := hs.Store.Load(t.Context()); err == nil {
		t.Fatal("Load must fail while the database cannot be opened")
	}

	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}
	if _, err := hs.Store.Load(t.Context()); err != nil {
		t.Fatalf("Load after the state directory recovered: %v", err)
	}
	if hs.Store.Degraded() {
		t.Fatal("degraded once open")
	}
	lc.RequireStart().RequireStop()
}

func TestNewHealthSetupWithoutInstanceLockRunsFromMemory(t *testing.T) {
	t.Parallel()

	path, stateDir := writeHealthConfig(t, "")
	// A directory where the lock file belongs: the lock cannot be taken, and
	// not because another instance holds it.
	if err := os.MkdirAll(filepath.Join(stateDir, instanceLockName), 0o700); err != nil {
		t.Fatal(err)
	}

	lc := fxtest.NewLifecycle(t)
	hs := newHealthSetup(lc, zap.NewNop(), &config{HealthConfig: path, HostName: "box"})
	lc.RequireStart().RequireStop()

	if hs.Config == nil {
		t.Fatal("monitoring must still run")
	}
	if hs.Store != nil {
		t.Fatal("without the instance lock the store must not be opened: two daemons must never share it")
	}
	if len(hs.Errors) != 1 || hs.Errors[0].Key != "health/lock/acquire" {
		t.Fatalf("errors = %+v", hs.Errors)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "health.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("health.db must not be created: %v", err)
	}
}

func TestNewHealthSetupUnparsableConfigDisablesHealth(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "health.yaml")
	if err := os.WriteFile(path, []byte("general: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	hs := newHealthSetup(fxtest.NewLifecycle(t), zap.NewNop(), &config{HealthConfig: path, HostName: "box"})
	if hs.Config != nil || len(hs.Errors) != 1 || hs.Errors[0].Key != "config/file/unreadable" {
		t.Fatalf("setup = %+v", hs)
	}
}

func TestNewHealthDisabledProvidesNothing(t *testing.T) {
	t.Parallel()

	pub := &fakePublisher{}
	lc := fxtest.NewLifecycle(t)
	out := newHealth(healthParams{
		Lifecycle: lc, Logger: zap.NewNop(), Config: &config{}, Setup: &healthSetup{Host: "box"},
		Publisher: pub, Uptime: sysfs.NewDefault(),
	})
	if len(out.Workers) != 0 || len(out.Handlers) != 0 {
		t.Fatalf("out = %+v", out)
	}
	lc.RequireStart().RequireStop()
	if got := pub.messages(); len(got) != 0 {
		t.Fatalf("a locked-out instance must stay silent: %v", got)
	}
}

func TestNewHealthUnparsableConfigReportsCheckerError(t *testing.T) {
	t.Parallel()

	pub := &fakePublisher{}
	lc := fxtest.NewLifecycle(t)
	out := newHealth(healthParams{
		Lifecycle: lc, Logger: zap.NewNop(), Config: &config{ChatID: 9},
		Setup: &healthSetup{
			Host:   "box",
			Errors: []startError{{Key: "config/file/unreadable", Text: "health.yaml: did not find expected node"}},
		},
		Publisher: pub, Uptime: sysfs.NewDefault(),
	})
	if len(out.Workers) != 0 || len(out.Handlers) != 0 {
		t.Fatalf("out = %+v", out)
	}
	lc.RequireStart().RequireStop()

	got := pub.messages()
	if len(got) != 1 || !strings.HasPrefix(got[0], "box: CHECKER ERROR: config: ") {
		t.Fatalf("messages = %v", got)
	}
}

func TestNewHealthEnabledRegistersRunnerAndCommands(t *testing.T) {
	t.Parallel()

	pub := &fakePublisher{}
	lc := fxtest.NewLifecycle(t)
	dir := t.TempDir()
	out := newHealth(healthParams{
		Lifecycle: lc, Logger: zap.NewNop(), Config: &config{ChatID: 9},
		Setup: &healthSetup{
			Host: "box",
			Config: &hcfg.Config{General: hcfg.General{
				StateDir: dir, HeartbeatFile: filepath.Join(dir, "heartbeat"), Concurrency: 1,
			}},
			Errors: []startError{{Key: "config/http/web-app/invalid", Text: "health.yaml: http/web-app: bad"}},
		},
		Publisher: pub, Uptime: sysfs.NewDefault(),
		Sets: []checkSet{{Errors: []startError{{Key: "ups/config/missing", Text: "no NUT"}}}},
	})
	if len(out.Workers) != 1 || out.Workers[0].Name() != runner.WorkerName {
		t.Fatalf("workers = %+v", out.Workers)
	}
	names := make([]string, 0, len(out.Handlers))
	for _, h := range out.Handlers {
		names = append(names, h.Name())
	}
	if !slices.Equal(names, []string{"status", "clear", "retire", "history", "temp", "disks"}) {
		t.Fatalf("handlers = %v", names)
	}

	lc.RequireStart().RequireStop()
	got := pub.messages()
	if len(got) != 2 || !strings.HasPrefix(got[0], "box: CHECKER ERROR: config: ") ||
		!strings.HasPrefix(got[1], "box: CHECKER ERROR: ups: ") {
		t.Fatalf("messages = %v", got)
	}
}

func TestNewConfigReadsHealthEnvironment(t *testing.T) {
	t.Setenv("BOT_HEALTH_CONFIG", "/etc/tgshd/health.yaml")
	t.Setenv("BOT_HOST_NAME", "box")
	t.Setenv("BOT_SMS_PHONE", "+10000000000")
	t.Setenv("BOT_SMTP_ADDR", "smtp.example.org:587")
	t.Setenv("BOT_SMTP_FROM", "tgshd@example.org")
	t.Setenv("BOT_SMTP_TO", "ops@example.org")
	t.Setenv("BOT_SMTP_USER", "user")
	t.Setenv("BOT_SMTP_PASS", "secret")

	cfg, err := newConfig()
	if err != nil {
		t.Fatal(err)
	}
	want := mail.Config{
		Addr: "smtp.example.org:587", From: "tgshd@example.org", To: "ops@example.org", User: "user", Pass: "secret",
	}
	if cfg.HealthConfig != "/etc/tgshd/health.yaml" || cfg.HostName != "box" || cfg.SMSPhone != "+10000000000" ||
		cfg.SMTP != want {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestAppOptionsGraphIsComplete(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  map[string]string
	}{
		{"plain", nil},
		{"health", map[string]string{"BOT_HEALTH_CONFIG": "/etc/tgshd/health.yaml"}},
		{"health with gammu", map[string]string{
			"BOT_HEALTH_CONFIG": "/etc/tgshd/health.yaml", "BOT_GAMMU_DSN": "postgres://gammu@db.example.org/gammu",
		}},
		{"everything", map[string]string{
			"BOT_HEALTH_CONFIG": "/etc/tgshd/health.yaml", "BOT_GAMMU_DSN": "postgres://gammu@db.example.org/gammu",
			"BOT_ZTE_HOST": "192.0.2.1",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			getenv := func(k string) string { return tt.env[k] }
			if err := fx.ValidateApp(appOptions(getenv)...); err != nil {
				t.Fatalf("fx graph: %v", err)
			}
		})
	}
}

// orderedPersister records its restore into a shared log.
type orderedPersister struct{ record func(string) }

func (p orderedPersister) Restore(store.State) error { p.record("restore"); return nil }
func (orderedPersister) Save(*store.State) error     { return nil }

func enabledHealthParams(t *testing.T, pub *fakePublisher, lc fx.Lifecycle, sets ...checkSet) healthParams {
	t.Helper()

	dir := t.TempDir()

	return healthParams{
		Lifecycle: lc, Logger: zap.NewNop(), Config: &config{ChatID: 9},
		Setup: &healthSetup{Host: "box", Config: &hcfg.Config{General: hcfg.General{
			StateDir: dir, HeartbeatFile: filepath.Join(dir, "heartbeat"), Concurrency: 1,
		}}},
		Publisher: pub, Uptime: sysfs.NewDefault(), Sets: sets,
	}
}

func TestNewHealthRestoresStateBeforeBackgroundJobsStart(t *testing.T) {
	t.Parallel()

	var (
		mu     sync.Mutex
		events []string
	)
	record := func(e string) {
		mu.Lock()
		defer mu.Unlock()

		events = append(events, e)
	}
	started := make(chan struct{})
	job := background{Key: "kernel/kmsg/read", Run: func(ctx context.Context) error {
		record("job")
		close(started)
		<-ctx.Done()

		return nil
	}}

	lc := fxtest.NewLifecycle(t)
	newHealth(enabledHealthParams(t, &fakePublisher{}, lc, checkSet{
		Persisters: []runner.Persister{orderedPersister{record: record}},
		Background: []background{job},
	}))
	lc.RequireStart()
	<-started
	lc.RequireStop()

	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(events, []string{"restore", "job"}) {
		t.Fatalf("events = %v, want the state restored before the job starts", events)
	}
}

// retireIndex returns the index of the /retire handler in out.Handlers.
func retireIndex(t *testing.T, out healthOut) int {
	t.Helper()

	i := slices.IndexFunc(out.Handlers, func(h telegram.Handler) bool { return h.Name() == "retire" })
	if i < 0 {
		t.Fatal("no /retire handler")
	}

	return i
}

func TestNewHealthRetireWithoutDisks(t *testing.T) {
	t.Parallel()

	pub := &fakePublisher{}
	out := newHealth(enabledHealthParams(t, pub, fxtest.NewLifecycle(t)))
	h := out.Handlers[retireIndex(t, out)]
	if !h.Enabled() {
		t.Fatal("/retire must stay enabled")
	}
	if err := h.Handle(t.Context(), 42, "/retire TEST"); err != nil {
		t.Fatal(err)
	}
	if got := pub.messages(); !slices.Equal(got, []string{"No drives are monitored."}) {
		t.Fatalf("reply = %q", got)
	}
	pub.mu.Lock()
	to := slices.Clone(pub.to)
	pub.mu.Unlock()
	if !slices.Equal(to, []int64{42}) {
		t.Fatalf("replied to %v, want the sender", to)
	}

	failing := newHealth(
		enabledHealthParams(t, &fakePublisher{err: errors.New("telegram down")}, fxtest.NewLifecycle(t)),
	)
	if err := failing.Handlers[retireIndex(t, failing)].Handle(t.Context(), 42, "/retire"); err == nil {
		t.Fatal("a failed reply must be returned")
	}
}

func TestNewHealthRetireWithDisks(t *testing.T) {
	t.Parallel()

	pub := &fakePublisher{}
	out := newHealth(enabledHealthParams(t, pub, fxtest.NewLifecycle(t), checkSet{Drives: &fakeDriveBook{}}))
	if err := out.Handlers[retireIndex(t, out)].Handle(t.Context(), 42, "/retire TEST"); err != nil {
		t.Fatal(err)
	}
	if got := pub.messages(); len(got) != 1 || !strings.HasPrefix(got[0], "Unknown drive.") {
		t.Fatalf("reply = %q, want the disks-backed /retire", got)
	}
}

// recordingSender records messages sent through it; it is safe for
// concurrent use.
type recordingSender struct {
	mu   sync.Mutex
	sent []string
}

func (s *recordingSender) Send(_ context.Context, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sent = append(s.sent, text)

	return nil
}

func (s *recordingSender) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.sent)
}

// fakeMailer stands in for the SMTP relay.
type fakeMailer struct{}

func (*fakeMailer) Send(context.Context, string, string) error { return nil }

// lockedSMS is a gammu stand-in that is safe for concurrent use.
type lockedSMS struct {
	mu   sync.Mutex
	sent []string
}

func (s *lockedSMS) Publish(_ context.Context, _, msg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sent = append(s.sent, msg)

	return nil
}

func (s *lockedSMS) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.sent)
}

// TestHealthConfigPausesChecksPerCheck: a loaded configuration keeps the
// hardware checks running in quiet windows by default, pauses every other
// check, and honours pause_in_quiet_windows set on a check (FR-026).
func TestHealthConfigPausesChecksPerCheck(t *testing.T) {
	t.Parallel()

	path, _ := writeHealthConfig(t, `network:
  internet_targets: [ "198.51.100.20" ]
disks: {}
temperatures:
  pause_in_quiet_windows: true
kernel:
  pause_in_quiet_windows: false
`)
	hc, errs := hcfg.Load(path)
	if hc == nil || len(errs) != 0 {
		t.Fatalf("Load() = %v, %v", hc, errs)
	}
	hs := &healthSetup{Config: hc}
	logger := zap.NewNop()

	probeSet, _ := buildModemProbeCheck(testModemProbeConfig(), nil, lookupPhone, time.Now)
	paused := map[string]bool{}
	for _, set := range []checkSet{
		networkChecks(hs, sysfs.NewDefault(), logger), disksChecks(hs, logger), tempsChecks(hs, newOpenState()), kernelChecks(hs, newContainerNames(), logger),
		probeSet,
	} {
		for _, c := range set.Checks {
			paused[c.Name] = c.PauseInQuiet
		}
	}
	want := map[string]bool{
		"network": true, disksCheckName: false, tempsCheckName: true, kernelCheckName: false,
		modemProbeCheckName: true,
	}
	for name, w := range want {
		got, ok := paused[name]
		if !ok || got != w {
			t.Errorf("%s: PauseInQuiet = %v (built %v), want %v", name, got, ok, w)
		}
	}
}

// failingCheckSet is one check that is bad on every run, alerting at once.
func failingCheckSet() checkSet {
	return checkSet{Checks: []health.Check{{
		Name: "ups", Interval: time.Second, Mode: health.ModeTelegram,
		Run: func(context.Context) health.CheckResult {
			return health.CheckResult{Readings: []health.Reading{{
				Key: "ups/power", Verdict: health.Bad, Text: "on battery",
			}}}
		},
	}}}
}

// TestNewHealthSendsQueuedProblemsBySMSWhileTelegramFails: with gammu and
// BOT_SMS_PHONE set, a PROBLEM that Telegram cannot take goes by SMS (FR-030).
func TestNewHealthSendsQueuedProblemsBySMSWhileTelegramFails(t *testing.T) {
	t.Parallel()

	pub := &fakePublisher{err: errors.New("telegram down")}
	gammu := &lockedSMS{}
	p := enabledHealthParams(t, pub, fxtest.NewLifecycle(t), failingCheckSet())
	p.Config.SMSPhone = "+10000000000"
	p.SMS = gammu

	out := newHealth(p)
	if err := out.Workers[0].Do(t.Context()); err != nil {
		t.Fatalf("Do() = %v", err)
	}

	got := gammu.messages()
	if len(got) != 1 || !strings.Contains(got[0], "PROBLEM") || !strings.Contains(got[0], "on battery") {
		t.Fatalf("SMS = %v, want the queued PROBLEM", got)
	}
}

// TestNewHealthWithoutSMSOrSMTPKeepsProblemsQueued: without SMS, SMTP or a
// store the daemon still runs; a PROBLEM Telegram cannot take stays queued
// and is delivered once Telegram recovers (Principle V).
func TestNewHealthWithoutSMSOrSMTPKeepsProblemsQueued(t *testing.T) {
	t.Parallel()

	pub := &fakePublisher{err: errors.New("telegram down")}
	p := enabledHealthParams(t, pub, fxtest.NewLifecycle(t), failingCheckSet())
	if p.Setup.Store != nil || p.SMS != nil || p.Config.SMTP.Enabled() {
		t.Fatalf("setup must have no store, SMS or SMTP: %+v", p)
	}

	out := newHealth(p)
	r := out.Workers[0]
	if err := r.Do(t.Context()); err != nil {
		t.Fatalf("Do() = %v", err)
	}

	pub.mu.Lock()
	pub.err = nil
	pub.mu.Unlock()
	if err := r.Do(t.Context()); err != nil {
		t.Fatalf("Do() = %v", err)
	}

	var problems int
	for _, m := range pub.messages() {
		if strings.Contains(m, "PROBLEM") && strings.Contains(m, "on battery") {
			problems++
		}
	}
	if problems != 2 {
		t.Fatalf("messages = %v; want the failed PROBLEM attempt, then its queued copy delivered", pub.messages())
	}
}

// TestStoreOpenRetriesUseTheShortBound: only the first open of the state
// database (at start, with its migrations) gets storeOpenTimeout; the retries
// made by the runner's Load each tick get storeRetryTimeout, so a hanging
// open never pushes a tick past the heartbeat budget.
func TestStoreOpenRetriesUseTheShortBound(t *testing.T) {
	t.Parallel()

	var limits []time.Duration
	open := boundedOpen(func(ctx context.Context) (*store.Store, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("open without a deadline")
		}
		limits = append(limits, time.Until(deadline))

		return nil, errors.New("busy")
	})
	for range 3 {
		_, _ = open(t.Context())
	}

	if len(limits) != 3 || limits[0] <= storeRetryTimeout || limits[0] > storeOpenTimeout {
		t.Fatalf("limits = %v, want the first bounded by %v", limits, storeOpenTimeout)
	}
	for _, l := range limits[1:] {
		if l > storeRetryTimeout {
			t.Fatalf("limits = %v, want every retry bounded by %v", limits, storeRetryTimeout)
		}
	}
}

// Memory guard wiring (T012).

// webAppID is the container id of web-app, as in its docker-<id>.scope.
const webAppID = "abababababababababababababababababababababababababababababababab"

// guardT0 is the start of the guard tests' virtual time.
func guardT0() time.Time { return time.Date(2026, 3, 1, 1, 0, 0, 0, time.UTC) }

// stepClock is the guard's clock in virtual time: each wait of one
// memguard.HostInterval advances it at once (after a short real pause, so the
// loop does not spin), every other wait (an action timeout) never fires.
type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *stepClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	if d != memguard.HostInterval {
		return ch
	}
	time.Sleep(100 * time.Microsecond)
	c.mu.Lock()

	c.now = c.now.Add(d)
	ch <- c.now
	c.mu.Unlock()

	return ch
}

// fakeGuardMemory is /proc as the guard reads it: in danger or healthy.
type fakeGuardMemory struct {
	mu     sync.Mutex
	danger bool
	psiErr error
	reads  int
	topN   []int
}

func (f *fakeGuardMemory) setDanger(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.danger = on
}

func (f *fakeGuardMemory) ReadMemInfo() (procmem.MemInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.reads++
	avail := uint64(50 << 30)
	if f.danger {
		avail = 2 << 30
	}

	return procmem.MemInfo{Total: 100 << 30, Available: avail}, nil
}

func (f *fakeGuardMemory) ReadPressure() (procmem.Pressure, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.psiErr != nil {
		return procmem.Pressure{}, f.psiErr
	}
	if f.danger {
		return procmem.Pressure{FullAvg10: 50, SomeAvg10: 60}, nil
	}

	return procmem.Pressure{}, nil
}

func (f *fakeGuardMemory) TopProcesses(n int) ([]procmem.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.topN = append(f.topN, n)

	return []procmem.Process{{PID: 4242, Name: "python3", RSS: 40 << 30, Cgroup: "/user.slice"}}, nil
}

func (f *fakeGuardMemory) hostReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.reads
}

// fakeGuardCgroups is cgroup v2 with web-app's container growing 2 GB per
// read, a flat backup.service and a container Docker does not know yet.
type fakeGuardCgroups struct {
	mu      sync.Mutex
	svcErr  error
	self    string
	selfErr error
	anon    uint64
	reads   int
	killed  []string
	killErr error
}

func (f *fakeGuardCgroups) Services() ([]cgroupmem.Service, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.svcErr != nil {
		return nil, f.svcErr
	}
	f.reads++
	f.anon += 2 << 30

	return []cgroupmem.Service{
		{
			Kind:    cgroupmem.KindUnit,
			Name:    "backup.service",
			Path:    "system.slice/backup.service",
			Anon:    1 << 30,
			Current: 1 << 30,
		},
		{
			Kind:    cgroupmem.KindContainer,
			Name:    webAppID,
			Path:    "system.slice/docker-" + webAppID + ".scope",
			Anon:    f.anon,
			Current: f.anon,
		},
		{
			Kind: cgroupmem.KindContainer, Name: strings.Repeat("cd", 32),
			Path: "system.slice/docker-" + strings.Repeat("cd", 32) + ".scope", Anon: 1 << 20,
		},
	}, nil
}

func (f *fakeGuardCgroups) serviceReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.reads
}

func (f *fakeGuardCgroups) Self() (string, error) {
	return f.self, f.selfErr
}

func (f *fakeGuardCgroups) Kill(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.killed = append(f.killed, path)

	return f.killErr
}

// fakeGuardContainers is Docker: the names it lists and the actions it took.
type fakeGuardContainers struct {
	mu      sync.Mutex
	names   map[string]string
	listErr error
	lists   int
	err     error
	calls   []string
}

func (f *fakeGuardContainers) ContainerNames(context.Context) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lists++
	if f.listErr != nil {
		return nil, f.listErr
	}

	return maps.Clone(f.names), nil
}

func (f *fakeGuardContainers) Restart(_ context.Context, name string) error {
	return f.do("restart " + name)
}

func (f *fakeGuardContainers) Stop(_ context.Context, name string) error {
	return f.do("stop " + name)
}

func (f *fakeGuardContainers) do(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, call)

	return f.err
}

func (f *fakeGuardContainers) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.calls)
}

func (f *fakeGuardContainers) listed() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.lists
}

// fakeGuardUnits is systemctl.
type fakeGuardUnits struct {
	mu    sync.Mutex
	err   error
	calls []string
}

func (f *fakeGuardUnits) Restart(_ context.Context, unit string) error {
	return f.do("restart " + unit)
}

func (f *fakeGuardUnits) Stop(_ context.Context, unit string) error { return f.do("stop " + unit) }

func (f *fakeGuardUnits) do(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, call)

	return f.err
}

// fakeShield counts its applications.
type fakeShield struct {
	mu      sync.Mutex
	err     error
	applied int
}

func (f *fakeShield) Apply() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.applied++

	return f.err
}

func (f *fakeShield) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.applied
}

// guardFakes are the fake host adapters of one guard.
type guardFakes struct {
	mem        *fakeGuardMemory
	cgroups    *fakeGuardCgroups
	containers *fakeGuardContainers
	units      *fakeGuardUnits
	shield     *fakeShield
	clock      *stepClock
	// names is the container id→name map the memory check refreshes.
	names *containerNames
}

func newGuardFakes(start time.Time) *guardFakes {
	return &guardFakes{
		names:      &containerNames{},
		mem:        &fakeGuardMemory{},
		cgroups:    &fakeGuardCgroups{self: "system.slice/tgshd.service"},
		containers: &fakeGuardContainers{names: map[string]string{webAppID: "web-app"}},
		units:      &fakeGuardUnits{},
		shield:     &fakeShield{},
		clock:      &stepClock{now: start},
	}
}

func (f *guardFakes) host() *guardHost {
	return &guardHost{
		mem: f.mem, cgroups: f.cgroups, containers: f.containers, units: f.units, shield: f.shield, clock: f.clock,
		names: f.names,
	}
}

// testMemorySection is a memory section whose messages all go to Telegram.
func testMemorySection(intervene hcfg.Intervene) *hcfg.Memory {
	return &hcfg.Memory{
		Mode: hcfg.ModeTLG,
		Guard: hcfg.Guard{
			Intervene: intervene, NeverTouch: []string{"db"},
			DangerAvailablePercent: 5, DangerPressurePercent: 20, DangerFor: hcfg.Duration(30 * time.Second),
			CulpritWindow: hcfg.Duration(time.Minute), CulpritMinGrowth: 1 << 30,
			RepeatWindow: hcfg.Duration(6 * time.Hour), RecoveryWindow: hcfg.Duration(10 * time.Second),
			ActionTimeout: hcfg.Duration(time.Minute), FailedMode: hcfg.ModeTLG,
		},
	}
}

// waitFor fails the test unless cond turns true within a few seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitNamed waits until the guard's refresh named the containers and the
// guard has read the services a few times since, so web-app has a history
// before danger.
func waitNamed(t *testing.T, f *guardFakes) {
	t.Helper()

	waitFor(t, "the container names", func() bool {
		name, ok := f.names.name(webAppID)
		return ok && name == "web-app"
	})
	reads := f.cgroups.serviceReads()
	waitFor(t, "the named services", func() bool { return f.cgroups.serviceReads() >= reads+3 })
}

// containing reports whether one of msgs contains sub.
func containing(msgs []string, sub string) bool {
	return slices.ContainsFunc(msgs, func(m string) bool { return strings.Contains(m, sub) })
}

// guardHealthParams are enabled health params with the memory section mem
// and the guard over fakes.
func guardHealthParams(
	t *testing.T,
	pub *fakePublisher,
	lc fx.Lifecycle,
	mem *hcfg.Memory,
	f *guardFakes,
) healthParams {
	t.Helper()

	p := enabledHealthParams(t, pub, lc)
	p.Setup.Config.Memory = mem
	p.Guard = f.host()

	return p
}

func TestNewMemoryGuardFollowsTheConfig(t *testing.T) {
	t.Parallel()

	host := newGuardFakes(guardT0()).host()
	if g := newMemoryGuard(nil, host, zap.NewNop()); g != nil {
		t.Fatal("no memory section: want no guard")
	}
	if g := newMemoryGuard(testMemorySection(hcfg.InterveneOff), host, zap.NewNop()); g != nil {
		t.Fatal("intervene off: want no guard")
	}
	if g := newMemoryGuard(testMemorySection(hcfg.InterveneAct), nil, zap.NewNop()); g != nil {
		t.Fatal("no host adapters: want no guard")
	}

	g := newMemoryGuard(testMemorySection(hcfg.InterveneAct), host, zap.NewNop())
	if g == nil {
		t.Fatal("intervene act: want a guard")
	}
	want := memguard.Config{
		CulpritWindow: time.Minute, CulpritMinGrowth: 1 << 30, NeverTouch: []string{"db"},
		Mode: memguard.ModeAct, DangerAvailablePercent: 5, DangerPressurePercent: 20, DangerFor: 30 * time.Second,
		RepeatWindow: 6 * time.Hour, RecoveryWindow: 10 * time.Second, ActionTimeout: time.Minute,
		Channel: health.Mode(hcfg.ModeTLG), FailedChannel: health.Mode(hcfg.ModeTLG),
	}
	if !reflect.DeepEqual(g.cfg, want) {
		t.Fatalf("cfg = %+v, want %+v", g.cfg, want)
	}
	if g := newMemoryGuard(testMemorySection(hcfg.InterveneReport), host, zap.NewNop()); g == nil ||
		g.cfg.Mode != memguard.ModeReport {
		t.Fatalf("intervene report: guard = %+v", g)
	}
}

func TestNewHealthStartsTheGuardOnlyWithTheSectionAndIntervene(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name  string
		mem   *hcfg.Memory
		start bool
	}{
		{"no section", nil, false},
		{"intervene off", testMemorySection(hcfg.InterveneOff), false},
		{"intervene report", testMemorySection(hcfg.InterveneReport), true},
		{"intervene act", testMemorySection(hcfg.InterveneAct), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newGuardFakes(guardT0())
			lc := fxtest.NewLifecycle(t)
			newHealth(guardHealthParams(t, &fakePublisher{}, lc, tt.mem, f))
			lc.RequireStart()
			if tt.start {
				waitFor(t, "the guard loop", func() bool { return f.mem.hostReads() > 1 })
			} else {
				time.Sleep(20 * time.Millisecond)
			}
			lc.RequireStop()

			if started := f.shield.count() == 1 && f.mem.hostReads() > 0; started != tt.start {
				t.Fatalf("shield applied %d, host reads %d; want started = %v",
					f.shield.count(), f.mem.hostReads(), tt.start)
			}
		})
	}
}

func TestGuardSamplerMapsContainerIDsToNames(t *testing.T) {
	t.Parallel()

	f := newGuardFakes(guardT0())
	names := &containerNames{}
	s := guardSampler{mem: f.mem, cgroups: f.cgroups, names: names}

	got, err := s.Services(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "backup.service" || got[0].Kind != memguard.KindUnit {
		t.Fatalf("before a refresh: samples = %+v, want only the unit (containers wait for their name)", got)
	}

	if err = names.refresh(t.Context(), f.containers); err != nil {
		t.Fatal(err)
	}
	got, err = s.Services(got[:0])
	if err != nil {
		t.Fatal(err)
	}
	want := []memguard.Sample{
		{
			Kind:       memguard.KindUnit,
			Name:       "backup.service",
			CgroupPath: "system.slice/backup.service",
			Anon:       1 << 30,
			Current:    1 << 30,
		},
		{
			Kind: memguard.KindContainer, Name: "web-app", CgroupPath: "system.slice/docker-" + webAppID + ".scope",
			Anon: 4 << 30, Current: 4 << 30,
		},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("samples = %+v, want %+v (the unknown container skipped)", got, want)
	}

	f.containers.listErr = errors.New("docker down")
	if err := names.refresh(t.Context(), f.containers); err == nil {
		t.Fatal("a failed list must be returned")
	}
	if name, ok := names.name(webAppID); !ok || name != "web-app" {
		t.Fatalf("after a failed refresh: name = %q, %v; want the last known map kept", name, ok)
	}

	f.cgroups.svcErr = errors.New("no system.slice")
	if _, err := s.Services(nil); err == nil {
		t.Fatal("a failed cgroup read must be returned")
	}
}

func TestGuardSamplerHostAndTopProcess(t *testing.T) {
	t.Parallel()

	f := newGuardFakes(guardT0())
	f.mem.setDanger(true)
	s := guardSampler{mem: f.mem, cgroups: f.cgroups, names: &containerNames{}}

	h, err := s.Host()
	if err != nil || h.AvailablePct != 2 || h.FullAvg10 != 50 {
		t.Fatalf("Host() = %+v, %v; want 2%% available, 50%% stalled", h, err)
	}
	top, err := s.TopProcess()
	want := memguard.Top{Name: "python3", PID: 4242, Size: 40 << 30, Slice: "/user.slice"}
	if err != nil || top != want {
		t.Fatalf("TopProcess() = %+v, %v; want %+v", top, err, want)
	}
	if !slices.Equal(f.mem.topN, []int{1}) {
		t.Fatalf("top processes asked for %v, want just the largest one", f.mem.topN)
	}

	f.mem.psiErr = procmem.ErrNoPressure
	if _, err := s.Host(); !errors.Is(err, procmem.ErrNoPressure) {
		t.Fatalf("no PSI: err = %v", err)
	}
}

func TestGuardActuatorRoutesByKindAndMapsUnreachable(t *testing.T) {
	t.Parallel()

	f := newGuardFakes(guardT0())
	a := guardActuator{containers: f.containers, units: f.units, cgroups: f.cgroups}
	ctr := memguard.Service{Kind: memguard.KindContainer, Name: "web-app"}
	unit := memguard.Service{Kind: memguard.KindUnit, Name: "backup.service"}

	for _, err := range []error{
		a.Restart(t.Context(), ctr), a.Stop(t.Context(), ctr), a.Restart(t.Context(), unit), a.Stop(t.Context(), unit),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := f.containers.actions(); !slices.Equal(got, []string{"restart web-app", "stop web-app"}) {
		t.Fatalf("docker calls = %v", got)
	}
	if got := f.units.calls; !slices.Equal(got, []string{"restart backup.service", "stop backup.service"}) {
		t.Fatalf("systemctl calls = %v", got)
	}

	for _, tt := range []struct {
		name        string
		ctrErr      error
		unitErr     error
		unreachable bool
	}{
		{"docker client unavailable", fmt.Errorf("docker: %w", errDockerUnavailable), nil, true},
		{"docker connection failed", fmt.Errorf("docker: %w", noDockerDaemon(t)), nil, true},
		{"container error", errors.New("no such container"), nil, false},
		{"no systemctl", nil, fmt.Errorf("systemctl restart backup.service: %w", exec.ErrNotFound), true},
		{"no system bus", nil, errors.New("systemctl restart backup.service: exit status 1: Failed to connect to bus: No such file or directory"), true},
		{"unit failed", nil, errors.New("systemctl restart backup.service: exit status 1: Job failed"), false},
	} {
		f.containers.err, f.units.err = tt.ctrErr, tt.unitErr
		s, want := ctr, tt.ctrErr
		if tt.unitErr != nil {
			s, want = unit, tt.unitErr
		}
		err := a.Restart(t.Context(), s)
		if !errors.Is(err, want) || errors.Is(err, memguard.ErrUnreachable) != tt.unreachable {
			t.Errorf("%s: err = %v, want it wrapped, unreachable = %v", tt.name, err, tt.unreachable)
		}
	}
}

// noDockerDaemon is the error of the Engine API client when nothing listens
// on its socket.
func noDockerDaemon(t *testing.T) error {
	t.Helper()

	c, err := dockerclient.New(dockerclient.WithHost("unix://" + filepath.Join(t.TempDir(), "docker.sock")))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.ContainerRestart(t.Context(), "web-app", dockerclient.ContainerRestartOptions{})
	if !dockerclient.IsErrConnectionFailed(err) {
		t.Fatalf("restart without a daemon: err = %v, want a connection failure", err)
	}

	return err
}

func TestGuardActuatorKillsOnlyServicesAndScopes(t *testing.T) {
	t.Parallel()

	f := newGuardFakes(guardT0())
	a := guardActuator{containers: f.containers, units: f.units, cgroups: f.cgroups}
	for _, p := range []string{"system.slice", "system.slice/app.slice", "", "system.slice/backup.service/..", "user.slice"} {
		if err := a.Kill(p); err == nil {
			t.Errorf("Kill(%q) must be refused", p)
		}
	}
	for _, p := range []string{"system.slice/backup.service", "system.slice/docker-" + webAppID + ".scope"} {
		if err := a.Kill(p); err != nil {
			t.Errorf("Kill(%q) = %v", p, err)
		}
	}
	want := []string{"system.slice/backup.service", "system.slice/docker-" + webAppID + ".scope"}
	if !slices.Equal(f.cgroups.killed, want) {
		t.Fatalf("killed %v, want only %v", f.cgroups.killed, want)
	}
}

// kvStore is a runner.Store that keeps only the kv; its first failPuts
// PutKV calls fail.
type kvStore struct {
	mu        sync.Mutex
	kv        map[string]string
	failPuts  int
	puts      int
	failLoads int
	loads     int
}

func (s *kvStore) PutKV(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.puts++
	if s.puts <= s.failPuts {
		return errors.New("database is locked")
	}
	if s.kv == nil {
		s.kv = map[string]string{}
	}
	s.kv[key] = value

	return nil
}

func (s *kvStore) Load(context.Context) (store.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.loads++
	if s.loads <= s.failLoads {
		return store.State{}, errors.New("database is locked")
	}

	return store.State{KV: maps.Clone(s.kv)}, nil
}

func (s *kvStore) Checkpoint(_ context.Context, dirty store.State, _ []store.Alert, _ []store.SampleRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.kv == nil {
		s.kv = map[string]string{}
	}
	maps.Copy(s.kv, dirty.KV)

	return nil
}

func (*kvStore) Degraded() bool { return false }

func (s *kvStore) value(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.kv[key]
}

// guardDaemon runs one daemon's runner and guard over st and f until stopped,
// and returns once the guard watches the named containers.
func guardDaemon(t *testing.T, st *kvStore, f *guardFakes) (*runner.Runner, *fxtest.Lifecycle) {
	t.Helper()

	g := newMemoryGuard(testMemorySection(hcfg.InterveneAct), f.host(), zap.NewNop())
	g.kv = st
	r := runner.New(runner.Options{
		Store: st, Persisters: []runner.Persister{g.persister()}, Concurrency: 1,
		Telegram: &recordingSender{}, Mode: notify.ModeTelegram, Logger: zap.NewNop(),
	})
	lc := fxtest.NewLifecycle(t)
	registerStart(lc, r.Load, startLoadTimeout, r.CheckerError, nil)
	g.register(lc, r)
	lc.RequireStart()
	// Danger only once Docker named the containers, so web-app is watched.
	waitNamed(t, f)

	return r, lc
}

// TestGuardRecordsSurviveARestartRightAfterAnAction: the record of an action
// reaches the store as soon as the action completes, with no tick checkpoint
// in between, so a daemon restarted right after it keeps the repeat history.
func TestGuardRecordsSurviveARestartRightAfterAnAction(t *testing.T) {
	t.Parallel()

	st := &kvStore{}
	first := newGuardFakes(guardT0())
	_, lc := guardDaemon(t, st, first)
	first.mem.setDanger(true)
	waitFor(t, "the restart", func() bool { return len(first.containers.actions()) > 0 })
	waitFor(t, "the record in the store, with no tick", func() bool {
		return strings.Contains(st.value(memoryGuardKey), `"name":"web-app"`)
	})
	lc.RequireStop()
	if got := first.containers.actions(); !slices.Equal(got, []string{"restart web-app"}) {
		t.Fatalf("first daemon: docker calls = %v", got)
	}

	// The restarted daemon remembers the restart: the same runaway within
	// repeat_window is stopped, not restarted again.
	second := newGuardFakes(guardT0().Add(3 * time.Hour))
	_, lc = guardDaemon(t, st, second)
	second.mem.setDanger(true)
	waitFor(t, "the second action", func() bool { return len(second.containers.actions()) > 0 })
	lc.RequireStop()
	if got := second.containers.actions(); !slices.Equal(got, []string{"stop web-app"}) {
		t.Fatalf("second daemon: docker calls = %v, want the restored record to make it a stop", got)
	}
}

// TestGuardUsesRecordsLoadedAfterItStarted: when the store cannot be read at
// start the guard starts without its records; once a later tick loads them,
// the running guard uses them, so a service it restarted before is stopped,
// not restarted again, and the store keeps the restored record.
func TestGuardUsesRecordsLoadedAfterItStarted(t *testing.T) {
	t.Parallel()

	earlier := guardRecord("web-app", guardT0().Add(-time.Hour))
	data, err := encodeRecords([]memguard.Record{earlier})
	if err != nil {
		t.Fatal(err)
	}
	st := &kvStore{kv: map[string]string{memoryGuardKey: data}, failLoads: 1}
	f := newGuardFakes(guardT0())
	r, lc := guardDaemon(t, st, f)
	if err := r.Load(t.Context()); err != nil {
		t.Fatalf("the later load: %v", err)
	}
	f.mem.setDanger(true)
	waitFor(t, "the action", func() bool { return len(f.containers.actions()) > 0 })
	waitFor(t, "the stop in the store", func() bool {
		return strings.Contains(st.value(memoryGuardKey), `"action":"stop"`)
	})
	lc.RequireStop()

	if got := f.containers.actions(); !slices.Equal(got, []string{"stop web-app"}) {
		t.Fatalf("docker calls = %v, want the late record to make it a stop", got)
	}
	var stored []memguard.Record
	if err := json.Unmarshal([]byte(st.value(memoryGuardKey)), &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 || !stored[0].Equal(earlier) || stored[1].Action != memguard.ActionStop {
		t.Fatalf("stored %+v, want the restored restart and the stop", stored)
	}
}

// TestMemoryGuardRetriesAFailedRecordWrite: a write the store refuses is
// retried every tick, not left for the next action, and warned about once.
func TestMemoryGuardRetriesAFailedRecordWrite(t *testing.T) {
	t.Parallel()

	core, logs := observer.New(zap.WarnLevel)
	st := &kvStore{failPuts: 2}
	host := newGuardFakes(guardT0()).host()
	host.clock = tickClock{}
	g := newMemoryGuard(testMemorySection(hcfg.InterveneAct), host, zap.New(core))
	g.kv = st
	if err := g.state.Restore(""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.writeRecords(ctx)
	}()

	g.state.Save(t.Context(), []memguard.Record{guardRecord("web-app", guardT0())})
	waitFor(t, "the record in the store after the failed writes", func() bool {
		return strings.Contains(st.value(memoryGuardKey), `"name":"web-app"`)
	})
	cancel()
	<-done

	if n := logs.FilterMessageSnippet("Memory guard cannot write its records").Len(); n != 1 {
		t.Fatalf("warnings = %d, want one while the writes keep failing", n)
	}
}

func guardRecord(name string, at time.Time) memguard.Record {
	return memguard.Record{
		ServiceID: "container:" + name, Name: name, Action: memguard.ActionRestart, At: at, Before: 1 << 30,
		Mode: memguard.ModeAct,
	}
}

// TestMemoryGuardWritesItsRecordsOnStop: records the loop saved but that were
// not written yet are written when the guard stops.
func TestMemoryGuardWritesItsRecordsOnStop(t *testing.T) {
	t.Parallel()

	st := &kvStore{}
	g := newMemoryGuard(testMemorySection(hcfg.InterveneAct), newGuardFakes(guardT0()).host(), zap.NewNop())
	g.kv = st
	if err := g.state.Restore(""); err != nil {
		t.Fatal(err)
	}
	g.state.Save(t.Context(), []memguard.Record{guardRecord("web-app", guardT0())})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	g.run(ctx, &blockingRunner{release: closedChan()})

	if !strings.Contains(st.value(memoryGuardKey), `"name":"web-app"`) {
		t.Fatalf("stored %q, want the record written on stop", st.value(memoryGuardKey))
	}
}

// TestMemoryGuardDoesNotWriteOverUnreadRecords: until the stored records were
// read, the guard's own never replace them.
func TestMemoryGuardDoesNotWriteOverUnreadRecords(t *testing.T) {
	t.Parallel()

	st := &kvStore{kv: map[string]string{memoryGuardKey: `[{"name":"db"}]`}}
	g := newMemoryGuard(testMemorySection(hcfg.InterveneAct), newGuardFakes(guardT0()).host(), zap.NewNop())
	g.kv = st
	g.state.Save(t.Context(), []memguard.Record{guardRecord("web-app", guardT0())})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	g.run(ctx, &blockingRunner{release: closedChan()})

	if got := st.value(memoryGuardKey); got != `[{"name":"db"}]` {
		t.Fatalf("stored %q, want the unread records kept", got)
	}
}

// TestGuardStateHandsRecordsRestoredLateToTheLoop: records restored after the
// loop started go to the running loop, and are kept beside the loop's own,
// once each and pruned by repeat_window, until the loop saved them.
func TestGuardStateHandsRecordsRestoredLateToTheLoop(t *testing.T) {
	t.Parallel()

	clock := &stepClock{now: guardT0().Add(2 * time.Hour)}
	s := newGuardState(zap.NewNop(), clock, 6*time.Hour)
	var handed [][]memguard.Record
	s.start(func(r []memguard.Record) { handed = append(handed, r) })
	own := guardRecord("web-app", guardT0().Add(time.Hour))
	s.Save(t.Context(), []memguard.Record{own})
	stored := guardRecord("worker", guardT0())
	expired := guardRecord("cache", guardT0().Add(-5*time.Hour))
	data, err := encodeRecords([]memguard.Record{stored, own, expired})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Restore(data); err != nil {
		t.Fatal(err)
	}
	if len(handed) != 1 || len(handed[0]) != 3 {
		t.Fatalf("handed %+v, want the three restored records once", handed)
	}
	// A save the loop queued before it merged them keeps them.
	s.Save(t.Context(), []memguard.Record{own})
	clock.now = clock.now.Add(4 * time.Hour) // expired leaves repeat_window

	if got := exportedRecords(t, s); len(got) != 2 || !got[0].Equal(stored) || !got[1].Equal(own) {
		t.Fatalf("exported %+v, want %+v and %+v", got, stored, own)
	}
	if last, ok := s.last(); !ok || last.Name != "web-app" {
		t.Fatalf("last = %+v, %v", last, ok)
	}

	// Once the loop saved its merged records the late ones are dropped.
	s.Save(t.Context(), []memguard.Record{stored, own})
	if s.late != nil {
		t.Fatalf("late = %+v, want none once the loop holds them", s.late)
	}
	if got := exportedRecords(t, s); len(got) != 2 {
		t.Fatalf("exported %+v, want the loop's two records", got)
	}
}

// exportedRecords decodes what s exports for the store.
func exportedRecords(t *testing.T, s *guardState) []memguard.Record {
	t.Helper()

	out, err := s.Export()
	if err != nil {
		t.Fatal(err)
	}
	var got []memguard.Record
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}

	return got
}

func TestGuardStateKV(t *testing.T) {
	t.Parallel()

	s := newGuardState(zap.NewNop(), &stepClock{now: guardT0()}, 6*time.Hour)
	if err := s.Restore(""); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Export(); err != nil || got != "[]" {
		t.Fatalf("empty Export() = %q, %v", got, err)
	}
	rec := []memguard.Record{{
		ServiceID: "container:web-app", Name: "web-app", Action: memguard.ActionRestart, At: guardT0(), Before: 1 << 30,
		Mode: memguard.ModeAct,
	}}
	s.Save(t.Context(), rec)
	data, err := s.Export()
	if err != nil {
		t.Fatal(err)
	}
	other := newGuardState(zap.NewNop(), &stepClock{now: guardT0()}, 6*time.Hour)
	if err := other.Restore(data); err != nil {
		t.Fatal(err)
	}
	if got := other.restoredRecords(); !reflect.DeepEqual(got, rec) {
		t.Fatalf("restored %+v, want %+v", got, rec)
	}
	if err := other.Restore("{"); err == nil {
		t.Fatal("a corrupt value must fail the restore")
	}
}

func TestMemoryGuardActsWhileMaintenanceIsHeld(t *testing.T) {
	t.Parallel()

	pub := &fakePublisher{}
	f := newGuardFakes(guardT0())
	lc := fxtest.NewLifecycle(t)
	p := guardHealthParams(t, pub, lc, testMemorySection(hcfg.InterveneAct), f)
	lock := filepath.Join(t.TempDir(), "maintenance.lock")
	p.Setup.Config.General.MaintenanceLock = lock
	held, err := os.Create(lock)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	out := newHealth(p)
	r, ok := out.Workers[0].(*runner.Runner)
	if !ok {
		t.Fatalf("worker = %T", out.Workers[0])
	}
	lc.RequireStart()
	if err := r.Do(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.Snapshot().Quiet == "" {
		t.Fatal("the maintenance lock is held: want a quiet window")
	}
	waitNamed(t, f)
	f.mem.setDanger(true)
	waitFor(t, "the restart", func() bool { return len(f.containers.actions()) > 0 })
	waitFor(t, "the restart message", func() bool {
		return containing(pub.messages(), "memory: restarted container web-app")
	})
	lc.RequireStop()
}

func TestMemoryGuardFallsBackToReportWithoutItsOwnCgroup(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		self    string
		selfErr error
	}{
		{"root cgroup", "", nil},
		{"unreadable", "", errors.New("read self cgroup: permission denied")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pub := &fakePublisher{}
			f := newGuardFakes(guardT0())
			f.cgroups.self, f.cgroups.selfErr = tt.self, tt.selfErr
			lc := fxtest.NewLifecycle(t)
			newHealth(guardHealthParams(t, pub, lc, testMemorySection(hcfg.InterveneAct), f))
			lc.RequireStart()
			waitNamed(t, f)
			f.mem.setDanger(true)
			waitFor(t, "the preview", func() bool {
				return containing(pub.messages(), "memory: would restart container web-app")
			})
			lc.RequireStop()

			if got := f.containers.actions(); len(got) != 0 {
				t.Fatalf("docker calls = %v, want none in report mode", got)
			}
			if !containing(pub.messages(), "CHECKER ERROR: memory: cannot tell tgshd") ||
				!containing(pub.messages(), "own cgroup") ||
				!containing(pub.messages(), "intervene is report") {
				t.Fatalf("messages = %v, want the fallback announced", pub.messages())
			}
		})
	}
}

func TestMemoryGuardWithoutCgroupV2RaisesAndDoesNotStart(t *testing.T) {
	t.Parallel()

	pub := &fakePublisher{}
	f := newGuardFakes(guardT0())
	f.cgroups.svcErr = errors.New("read system.slice: no such file or directory")
	lc := fxtest.NewLifecycle(t)
	newHealth(guardHealthParams(t, pub, lc, testMemorySection(hcfg.InterveneAct), f))
	lc.RequireStart()
	waitFor(t, "the checker error", func() bool { return containing(pub.messages(), "cgroup v2") })
	lc.RequireStop()

	if n := f.mem.hostReads(); n != 0 {
		t.Fatalf("host reads = %d, want no guard loop", n)
	}
}

func TestMemoryGuardWithoutPressureRaisesAndDoesNotStart(t *testing.T) {
	t.Parallel()

	pub := &fakePublisher{}
	f := newGuardFakes(guardT0())
	f.mem.psiErr = procmem.ErrNoPressure
	lc := fxtest.NewLifecycle(t)
	newHealth(guardHealthParams(t, pub, lc, testMemorySection(hcfg.InterveneAct), f))
	lc.RequireStart()
	waitFor(t, "the checker error", func() bool { return containing(pub.messages(), "CHECKER ERROR: memory: ") })
	time.Sleep(20 * time.Millisecond)
	lc.RequireStop()

	if n := f.mem.hostReads(); n != 1 {
		t.Fatalf("host reads = %d, want only the start probe", n)
	}
}

func TestMemoryGuardRaisesEachShieldFailure(t *testing.T) {
	t.Parallel()

	pub := &fakePublisher{}
	f := newGuardFakes(guardT0())
	f.shield.err = errors.Join(
		errors.New("set oom_score_adj: permission denied"),
		errors.New("mlockall: operation not permitted"),
	)
	lc := fxtest.NewLifecycle(t)
	newHealth(guardHealthParams(t, pub, lc, testMemorySection(hcfg.InterveneAct), f))
	lc.RequireStart()
	waitFor(t, "both checker errors", func() bool {
		msgs := pub.messages()

		return containing(msgs, "oom_score_adj") && containing(msgs, "mlockall")
	})
	waitFor(t, "the guard loop", func() bool { return f.mem.hostReads() > 1 })
	lc.RequireStop()
	for _, m := range pub.messages() {
		if strings.Contains(m, "oom_score_adj") && strings.Contains(m, "mlockall") {
			t.Fatalf("message %q: want one checker error per failure", m)
		}
	}
}

// blockingRunner is a guardRunner whose checker errors hang until released,
// as over a network that is down.
type blockingRunner struct {
	release chan struct{}
	mu      sync.Mutex
	raised  []string
}

func (b *blockingRunner) PushMode(context.Context, string, string, string, string, health.Mode) {}

func (b *blockingRunner) CheckerError(key, _ string) {
	<-b.release
	b.mu.Lock()
	defer b.mu.Unlock()

	b.raised = append(b.raised, key)
}

func (b *blockingRunner) keys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return slices.Clone(b.raised)
}

func TestMemoryGuardStartsWhileCheckerErrorsHang(t *testing.T) {
	t.Parallel()

	f := newGuardFakes(guardT0())
	f.shield.err = errors.Join(
		errors.New("set oom_score_adj: permission denied"),
		errors.New("mlockall: operation not permitted"),
	)
	f.cgroups.self = ""
	g := newMemoryGuard(testMemorySection(hcfg.InterveneAct), f.host(), zap.NewNop())
	r := &blockingRunner{release: make(chan struct{})}
	lc := fxtest.NewLifecycle(t)
	g.register(lc, r)
	lc.RequireStart()

	waitFor(t, "the guard loop while the checker errors hang", func() bool { return f.mem.hostReads() > 3 })
	close(r.release)
	want := []string{guardShieldKey + "/1", guardShieldKey + "/2", guardSelfKey}
	waitFor(t, "the checker errors", func() bool { return slices.Equal(r.keys(), want) })
	lc.RequireStop()
}

// testMemoryCheckSection is a memory section with its warning levels.
func testMemoryCheckSection() *hcfg.Memory {
	m := testMemorySection(hcfg.InterveneReport)
	m.Interval, m.Threshold, m.PauseInQuietWindows = hcfg.Duration(30*time.Second), hcfg.Duration(time.Minute), true
	m.WarnAvailablePercent, m.WarnSwapPercent, m.WarnPressurePercent = 15, 80, 10
	m.GrowthWindow, m.GrowthWarn = hcfg.Duration(time.Hour), 4<<30

	return m
}

// testMemoryCheck builds the memory check over the fakes.
func testMemoryCheck(f *guardFakes, logger *zap.Logger) checkSet {
	src := memorySource{sampler: guardSampler{mem: f.mem, cgroups: f.cgroups, names: f.names}}

	return buildMemoryCheck(testMemoryCheckSection(), src, f.containers, logger)
}

func TestMemoryChecksBuiltOnlyWithTheSection(t *testing.T) {
	t.Parallel()

	names := &containerNames{}
	for name, hs := range map[string]*healthSetup{"disabled": {}, "no section": {Config: &hcfg.Config{}}} {
		set := memoryChecks(hs, &lazyDocker{}, names, zap.NewNop())
		if len(set.Checks) != 0 || len(set.Persisters) != 0 || set.Memory != nil {
			t.Fatalf("%s: set = %+v, want empty", name, set)
		}
	}

	set := memoryChecks(&healthSetup{Config: &hcfg.Config{Memory: testMemoryCheckSection()}}, &lazyDocker{}, names,
		zap.NewNop())
	if len(set.Checks) != 1 || set.Memory == nil || len(set.Errors) != 0 {
		t.Fatalf("set = %+v, want the memory check", set)
	}
	c := set.Checks[0]
	if c.Name != memoryCheckName || c.Interval != 30*time.Second || c.Threshold != time.Minute ||
		c.Mode != health.Mode(hcfg.ModeTLG) || !c.PauseInQuiet || c.NeedsInternet || c.DependsOn != "" {
		t.Fatalf("check = %+v", c)
	}
	if len(set.Persisters) != 2 {
		t.Fatalf("persisters = %+v, want two", set.Persisters)
	}
	for i, key := range []string{"memory.growth", "memory.audit"} {
		if p, ok := set.Persisters[i].(runner.KVPersister); !ok || p.Key != key {
			t.Fatalf("persisters[%d] = %+v, want kv %s", i, set.Persisters[i], key)
		}
	}
}

func TestMemoryCheckRefreshesTheContainerNames(t *testing.T) {
	t.Parallel()

	core, logs := observer.New(zap.WarnLevel)
	f := newGuardFakes(guardT0())
	c := testMemoryCheck(f, zap.New(core)).Checks[0]

	res := c.Run(t.Context())
	if f.containers.listed() != 1 {
		t.Fatalf("docker listed %d times, want once per run", f.containers.listed())
	}
	if name, ok := f.names.name(webAppID); !ok || name != "web-app" {
		t.Fatalf("name = %q, %v; want the guard's map refreshed by the run", name, ok)
	}
	keys := make([]string, 0, len(res.Readings))
	for _, r := range res.Readings {
		keys = append(keys, r.Key)
	}
	want := []string{
		"memory/available", "memory/pressure", "memory/growth/backup.service", "memory/growth/web-app",
	}
	if !slices.Equal(keys, want) || res.Err != nil {
		t.Fatalf("readings = %v, err = %v; want %v (the unnamed container left out)", keys, res.Err, want)
	}

	// Docker down: the run goes on with the last names, warned once.
	f.containers.mu.Lock()
	f.containers.listErr = errors.New("docker down")
	f.containers.mu.Unlock()
	for range 2 {
		if res := c.Run(t.Context()); res.Err != nil || len(res.Readings) != len(want) {
			t.Fatalf("with docker down: readings = %+v, err = %v", res.Readings, res.Err)
		}
	}
	if name, ok := f.names.name(webAppID); !ok || name != "web-app" {
		t.Fatalf("name = %q, %v; want the last map kept", name, ok)
	}
	if n := logs.FilterMessageSnippet("cannot list containers").Len(); n != 1 {
		t.Fatalf("warnings = %d, want one while the listing keeps failing", n)
	}
}

func TestMemorySourceReadsTheHost(t *testing.T) {
	t.Parallel()

	f := newGuardFakes(guardT0())
	f.mem.setDanger(true)
	src := memorySource{sampler: guardSampler{mem: f.mem, cgroups: f.cgroups, names: f.names}}

	host, err := src.Host(t.Context())
	want := memory.Host{Total: 100 << 30, Available: 2 << 30, FullAvg10: 50, HasPressure: true}
	if err != nil || host != want {
		t.Fatalf("Host() = %+v, %v; want %+v", host, err, want)
	}
	top, err := src.TopProcess(t.Context())
	if wantTop := (memory.Process{Name: "python3", PID: 4242, RSS: 40 << 30}); err != nil || top != wantTop {
		t.Fatalf("TopProcess() = %+v, %v; want %+v", top, err, wantTop)
	}
	svcs, err := src.Services(t.Context())
	if err != nil || len(svcs) != 1 || svcs[0] != (memory.Service{Name: "backup.service", Anon: 1 << 30}) {
		t.Fatalf("Services() = %+v, %v; want only the unit before Docker named the containers", svcs, err)
	}

	f.mem.psiErr = procmem.ErrNoPressure
	if host, err := src.Host(t.Context()); err != nil || host.HasPressure {
		t.Fatalf("without PSI: Host() = %+v, %v; want no pressure and no error", host, err)
	}
	f.mem.psiErr = errors.New("parse memory pressure: some or full line missing")
	if _, err := src.Host(t.Context()); err == nil {
		t.Fatal("an unreadable pressure file must fail the read")
	}
}

func TestMemoryGrowthSurvivesARestart(t *testing.T) {
	t.Parallel()

	f := newGuardFakes(guardT0())
	set := testMemoryCheck(f, zap.NewNop())
	set.Checks[0].Run(t.Context())
	var st store.State
	if err := set.Persisters[0].Save(&st); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.KV["memory.growth"], `"web-app"`) {
		t.Fatalf("kv = %v, want the growth history", st.KV)
	}

	restarted := testMemoryCheck(newGuardFakes(guardT0()), zap.NewNop())
	if err := restarted.Persisters[0].Restore(st); err != nil {
		t.Fatal(err)
	}
	var again store.State
	if err := restarted.Persisters[0].Save(&again); err != nil || again.KV["memory.growth"] != st.KV["memory.growth"] {
		t.Fatalf("restored kv = %v, %v; want %v", again.KV, err, st.KV)
	}
}

func TestMemoryGuardNamesContainersWithoutTheCheck(t *testing.T) {
	t.Parallel()

	// The daemon with the guard but no memory check run (paused by a quiet
	// window, or a long interval): the guard names the containers itself.
	f := newGuardFakes(guardT0())
	lc := fxtest.NewLifecycle(t)
	newHealth(guardHealthParams(t, &fakePublisher{}, lc, testMemorySection(hcfg.InterveneAct), f))
	lc.RequireStart()
	waitNamed(t, f)
	lc.RequireStop()
}

// tickClock fires every wait at once (after a short real pause).
type tickClock struct{}

func (tickClock) Now() time.Time { return guardT0() }

func (tickClock) After(time.Duration) <-chan time.Time {
	time.Sleep(100 * time.Microsecond)

	ch := make(chan time.Time, 1)
	ch <- guardT0()

	return ch
}

func TestMemoryGuardRefreshesNamesEveryTick(t *testing.T) {
	t.Parallel()

	core, logs := observer.New(zap.WarnLevel)
	f := newGuardFakes(guardT0())
	host := f.host()
	host.clock = tickClock{}
	g := newMemoryGuard(testMemorySection(hcfg.InterveneAct), host, zap.New(core))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.refreshNames(ctx)
	}()

	waitFor(t, "the first names", func() bool { _, ok := f.names.name(webAppID); return ok })
	// A container redeployed while no check runs is named by a later tick.
	f.containers.mu.Lock()
	f.containers.names = map[string]string{webAppID: "web-app-v2"}
	f.containers.mu.Unlock()
	waitFor(t, "the renamed container", func() bool { name, _ := f.names.name(webAppID); return name == "web-app-v2" })

	f.containers.mu.Lock()
	f.containers.listErr = errors.New("docker down")
	f.containers.mu.Unlock()
	listed := f.containers.listed()
	waitFor(t, "a few failing lists", func() bool { return f.containers.listed() > listed+3 })
	cancel()
	<-done

	if name, _ := f.names.name(webAppID); name != "web-app-v2" {
		t.Fatalf("name = %q, want the last map kept while Docker is down", name)
	}
	if n := logs.FilterMessageSnippet("Memory guard cannot list containers").Len(); n != 1 {
		t.Fatalf("warnings = %d, want one while the listing keeps failing", n)
	}
}

func TestMemoryGuardStatus(t *testing.T) {
	t.Parallel()

	f := newGuardFakes(guardT0())
	f.cgroups.self = "" // the root cgroup: act falls back to report
	g := newMemoryGuard(testMemorySection(hcfg.InterveneAct), f.host(), zap.NewNop())
	if s := g.status(); s.Mode != "act" || s.Last != nil {
		t.Fatalf("before the start: status = %+v, want the configured mode", s)
	}

	r := runner.New(runner.Options{
		Persisters: []runner.Persister{g.persister()}, Concurrency: 1,
		Telegram: &recordingSender{}, Mode: notify.ModeTelegram, Logger: zap.NewNop(),
	})
	lc := fxtest.NewLifecycle(t)
	g.register(lc, r)
	lc.RequireStart()
	waitNamed(t, f)
	f.mem.setDanger(true)
	waitFor(t, "the reported action", func() bool { return g.status().Last != nil })
	lc.RequireStop()

	s := g.status()
	if s.Mode != "report" || s.Last.Action != "restart" || s.Last.Service != "web-app" || !s.Last.Report ||
		s.Last.At.IsZero() {
		t.Fatalf("status = %+v, last = %+v; want the effective mode and the would-restart", s, s.Last)
	}

	broken := newGuardFakes(guardT0())
	broken.cgroups.svcErr = errors.New("no system.slice")
	g = newMemoryGuard(testMemorySection(hcfg.InterveneAct), broken.host(), zap.NewNop())
	g.run(t.Context(), &blockingRunner{release: closedChan()})
	if s := g.status(); s.Mode != "not running" {
		t.Fatalf("status = %+v, want not running", s)
	}
}

func closedChan() chan struct{} {
	c := make(chan struct{})
	close(c)

	return c
}

// fakeAuditor is the container list of the limit audit.
type fakeAuditor struct {
	list []memory.Container
	err  error
}

func (f fakeAuditor) AuditContainers(context.Context) ([]memory.Container, error) {
	return f.list, f.err
}

func TestMemoryCheckAuditsContainerLimits(t *testing.T) {
	t.Parallel()

	f := newGuardFakes(guardT0())
	section := testMemoryCheckSection()
	section.AuditExclude = []string{"scratch-job"}
	src := memorySource{
		sampler: guardSampler{mem: f.mem, cgroups: f.cgroups, names: f.names},
		containers: fakeAuditor{list: []memory.Container{
			{Name: "web-app", Running: true}, {Name: "scratch-job", Running: true},
		}},
	}
	c := buildMemoryCheck(section, src, f.containers, zap.NewNop()).Checks[0]

	res := c.Run(t.Context())

	var got []health.Reading
	for _, r := range res.Readings {
		if strings.HasPrefix(r.Key, "memory/limit/") {
			got = append(got, r)
		}
	}
	if len(got) != 1 || got[0].Key != "memory/limit/web-app" || got[0].Verdict != health.Bad {
		t.Fatalf("limit readings = %+v, want only web-app open (scratch-job excluded)", got)
	}
}

func TestMemorySourceContainers(t *testing.T) {
	t.Parallel()

	if _, err := (memorySource{}).Containers(t.Context()); !errors.Is(err, memory.ErrNoContainers) {
		t.Fatalf("no auditor: err = %v, want ErrNoContainers", err)
	}
	gone := memorySource{containers: fakeAuditor{err: fmt.Errorf("%w: boom", errDockerUnavailable)}}
	if _, err := gone.Containers(t.Context()); !errors.Is(err, memory.ErrNoContainers) {
		t.Fatalf("docker unavailable: err = %v, want ErrNoContainers", err)
	}
	other := errors.New("engine error")
	failing := memorySource{containers: fakeAuditor{err: other}}
	if _, err := failing.Containers(t.Context()); !errors.Is(err, other) || errors.Is(err, memory.ErrNoContainers) {
		t.Fatalf("engine error: err = %v, want it passed through", err)
	}
}
