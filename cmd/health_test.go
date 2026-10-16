package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/Toshik1978/tgshd/pkg/health"
	hcfg "github.com/Toshik1978/tgshd/pkg/health/config"
	"github.com/Toshik1978/tgshd/pkg/health/notify"
	"github.com/Toshik1978/tgshd/pkg/health/runner"
	"github.com/Toshik1978/tgshd/pkg/health/store"
	"github.com/Toshik1978/tgshd/pkg/host/lockfile"
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
		Interval:      hcfg.Duration(5 * time.Minute),
		Threshold:     hcfg.Duration(time.Minute),
		Mode:          hcfg.ModeTLGOrSMS,
		NeedsInternet: true,
	}
	ran := false
	chk := newCheck("ups", common, func(context.Context) health.CheckResult {
		ran = true

		return health.CheckResult{}
	})

	if chk.Name != "ups" || chk.Interval != 5*time.Minute || chk.Threshold != time.Minute ||
		chk.Mode != health.ModeTelegramOrSMS || !chk.NeedsInternet || chk.DependsOn != "" {
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
	if len(res.Readings) != 2 || res.Readings[0].Verdict != health.Good || res.Readings[1].Verdict != health.Bad {
		t.Fatalf("readings = %+v", res.Readings)
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
	opts = runnerOptions(hs, d, checkSet{Checks: checks}, sysfs.NewDefault(), zap.NewNop())
	if opts.Maintenance == nil {
		t.Fatal("maintenance lock configured: want a probe")
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
