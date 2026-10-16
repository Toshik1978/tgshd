package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/cmd/app"
	"github.com/Toshik1978/tgshd/pkg/command"
	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/modemprobe"
	hcfg "github.com/Toshik1978/tgshd/pkg/health/config"
	"github.com/Toshik1978/tgshd/pkg/health/store"
	"github.com/Toshik1978/tgshd/pkg/sms"
	"github.com/Toshik1978/tgshd/pkg/sms/gammu"
)

const (
	testModemPhone = "+10000000000"
	testGammuDSN   = "postgres://gammu@db.example.org/gammu"
)

// fakeInboxRepo is the gammu inbox repository.
type fakeInboxRepo struct {
	rows        []gammu.InboxMessage
	err         error
	marked      [][]int64
	hadDeadline bool
}

func (f *fakeInboxRepo) Unprocessed(ctx context.Context) ([]gammu.InboxMessage, error) {
	_, f.hadDeadline = ctx.Deadline()

	return f.rows, f.err
}

func (f *fakeInboxRepo) MarkProcessed(ctx context.Context, ids []int64) error {
	_, f.hadDeadline = ctx.Deadline()
	f.marked = append(f.marked, ids)

	return f.err
}

func TestGammuInboxAdaptsTheRepository(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	repo := &fakeInboxRepo{rows: []gammu.InboxMessage{
		{ID: 7, Sender: testModemPhone, Text: "hello", UDH: "050003010201", ReceivedAt: at},
	}}
	in := gammuInbox{repo: repo}

	got, err := in.Unprocessed(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []sms.InboxMessage{
		{ID: 7, SenderNumber: testModemPhone, TextDecoded: "hello", UDH: "050003010201", ReceivingDateTime: at},
	}
	if !slices.Equal(got, want) || !repo.hadDeadline {
		t.Fatalf("rows = %+v, deadline = %v", got, repo.hadDeadline)
	}

	repo.hadDeadline = false
	if err = in.MarkProcessed(t.Context(), []int64{7}); err != nil {
		t.Fatal(err)
	}
	if len(repo.marked) != 1 || !slices.Equal(repo.marked[0], []int64{7}) || !repo.hadDeadline {
		t.Fatalf("marked = %v, deadline = %v", repo.marked, repo.hadDeadline)
	}

	down := gammuInbox{repo: &fakeInboxRepo{err: errors.New("connection refused")}}
	if _, err = down.Unprocessed(t.Context()); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v", err)
	}
	if err = down.MarkProcessed(t.Context(), []int64{7}); err == nil {
		t.Fatal("a failed mark must be returned")
	}
}

func TestBoundedSMSSetsADeadline(t *testing.T) {
	t.Parallel()

	f := &fakeSMS{}
	if err := (boundedSMS{sender: f}).Publish(t.Context(), testModemPhone, "probe"); err != nil {
		t.Fatal(err)
	}
	if !f.hadDeadline || f.phone != testModemPhone || f.msg != "probe" {
		t.Fatalf("sms = %+v", f)
	}

	f.err = errors.New("outbox down")
	if err := (boundedSMS{sender: f}).Publish(t.Context(), testModemPhone, "probe"); err == nil {
		t.Fatal("a failed send must be returned")
	}
}

func testModemProbeConfig() *hcfg.ModemProbe {
	return &hcfg.ModemProbe{
		PhoneEnv: "MODEM_PHONE", Time: "11:02", Weekday: time.Sunday,
		ReplyWithin: hcfg.Duration(30 * time.Minute), MaxSilence: hcfg.Duration(8 * 24 * time.Hour),
	}
}

func TestModemProbeChecksFollowTheConfig(t *testing.T) {
	t.Setenv("TGSHD_TEST_PROBE_PHONE", testModemPhone)

	mp := testModemProbeConfig()
	mp.PhoneEnv = "TGSHD_TEST_PROBE_PHONE"
	withProbe := &healthSetup{Config: &hcfg.Config{ModemProbe: mp}}
	tests := []struct {
		name        string
		setup       *healthSetup
		dsn         string
		sms         *fakeSMS
		wantChecks  int
		wantErr     string
		wantMatcher bool
	}{
		{name: "health disabled", setup: &healthSetup{}, dsn: testGammuDSN, sms: &fakeSMS{}},
		{name: "no section", setup: &healthSetup{Config: &hcfg.Config{}}, dsn: testGammuDSN, sms: &fakeSMS{}},
		{name: "without gammu", setup: withProbe, wantErr: "modem_probe/config/missing"},
		{name: "daemon", setup: withProbe, dsn: testGammuDSN, sms: &fakeSMS{}, wantChecks: 1, wantMatcher: true},
		{name: "check --once", setup: withProbe, dsn: testGammuDSN, wantChecks: 1, wantMatcher: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sender command.SmsSender // a nil *fakeSMS must stay a nil interface
			if tt.sms != nil {
				sender = tt.sms
			}
			out := modemProbeChecks(modemProbeParams{Setup: tt.setup, Config: &config{GammuDSN: tt.dsn}, SMS: sender})
			if len(out.Set.Checks) != tt.wantChecks || len(out.Set.Persisters) != tt.wantChecks {
				t.Fatalf("checks = %d, persisters = %d", len(out.Set.Checks), len(out.Set.Persisters))
			}
			if (out.Matcher != nil) != tt.wantMatcher {
				t.Fatalf("matcher = %v", out.Matcher)
			}
			assertStartErrors(t, out.Set.Errors, tt.wantErr)
			if tt.wantChecks == 1 {
				assertProbeCheck(t, out.Set, tt.sms)
			}
		})
	}
}

// assertStartErrors checks the set raised exactly want (none when empty),
// naming BOT_GAMMU_DSN.
func assertStartErrors(t *testing.T, errs []startError, want string) {
	t.Helper()

	keys := make([]string, 0, len(errs))
	for _, e := range errs {
		keys = append(keys, e.Key)
	}
	if want == "" && len(keys) != 0 || want != "" && !slices.Equal(keys, []string{want}) {
		t.Fatalf("errors = %+v", errs)
	}
	if want != "" && !strings.Contains(errs[0].Text, "BOT_GAMMU_DSN") {
		t.Fatalf("error text = %q", errs[0].Text)
	}
}

// assertProbeCheck checks the probe's settings, then runs it on a stored
// state where a probe is due: the daemon's probe sends through sent, the
// check --once one (sent nil) has no sender and only reports.
func assertProbeCheck(t *testing.T, set checkSet, sent *fakeSMS) {
	t.Helper()

	chk := set.Checks[0]
	if chk.Name != "modem_probe" || chk.Interval != modemProbeInterval || chk.Threshold != 0 ||
		chk.Mode != health.Mode(hcfg.ModeTLG) {
		t.Fatalf("check = %+v", chk)
	}

	due := probeStore(t, modemprobe.State{FirstMissingAt: time.Now().Add(-7*24*time.Hour - time.Hour)})
	if err := set.Persisters[0].Restore(due); err != nil {
		t.Fatal(err)
	}
	res := chk.Run(t.Context())

	if sent != nil {
		if !strings.HasPrefix(sent.msg, "tgshd probe ") || sent.phone != testModemPhone {
			t.Fatalf("daemon probe: sms = %+v, result = %+v; want the due probe sent", sent, res)
		}

		return
	}
	if len(res.Readings) != 1 || res.Readings[0].Verdict != health.Unknown ||
		!strings.Contains(res.Readings[0].Text, "has not sent it") {
		t.Fatalf("check --once probe: %+v; want report-only", res)
	}
}

// probeStore is a stored state holding the given probe state.
func probeStore(t *testing.T, st modemprobe.State) store.State {
	t.Helper()

	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}

	return store.State{KV: map[string]string{modemProbeStateKey: string(data)}}
}

// sundayClock is a settable clock; 2026-10-18 is a Sunday.
type sundayClock struct{ now time.Time }

func (c *sundayClock) Now() time.Time { return c.now }

func lookupPhone(string) (string, bool) { return testModemPhone, true }

func TestBuildModemProbeCheckWaitsForItsStateThenSendsAndPersists(t *testing.T) {
	t.Parallel()

	f := &fakeSMS{}
	clk := &sundayClock{now: time.Date(2026, 10, 18, 11, 1, 0, 0, time.UTC)}
	set, probe := buildModemProbeCheck(testModemProbeConfig(), boundedSMS{sender: f}, lookupPhone, clk.Now)
	if probe == nil || len(set.Checks) != 1 || len(set.Persisters) != 1 {
		t.Fatalf("set = %+v", set)
	}
	run := set.Checks[0].Run

	if res := run(t.Context()); !errors.Is(res.Err, errProbeStateNotLoaded) {
		t.Fatalf("before restore: %+v", res)
	}

	// The stored state: the first run was a week ago, so this Sunday's probe is due at 11:02.
	stored := probeStore(t, modemprobe.State{FirstMissingAt: clk.now.AddDate(0, 0, -6)})
	if err := set.Persisters[0].Restore(stored); err != nil {
		t.Fatal(err)
	}
	if res := run(t.Context()); res.Err != nil || f.msg != "" {
		t.Fatalf("not due yet: %+v, sent %q", res, f.msg)
	}

	clk.now = clk.now.Add(time.Minute)
	if res := run(t.Context()); res.Err != nil || len(res.Readings) != 0 {
		t.Fatalf("due: %+v", res)
	}
	if f.phone != testModemPhone || !strings.HasPrefix(f.msg, "tgshd probe ") || !f.hadDeadline {
		t.Fatalf("sms = %+v", f)
	}

	var saved store.State
	if err := set.Persisters[0].Save(&saved); err != nil {
		t.Fatal(err)
	}
	var st modemprobe.State
	if err := json.Unmarshal([]byte(saved.KV[modemProbeStateKey]), &st); err != nil {
		t.Fatal(err)
	}
	if st.Token == "" || !strings.HasSuffix(f.msg, st.Token) {
		t.Fatalf("saved = %+v", st)
	}

	if !probe.Match(testModemPhone, f.msg, clk.now) {
		t.Fatal("the returned probe must match")
	}
	if res := run(t.Context()); len(res.Readings) != 1 || res.Readings[0].Verdict != health.Good {
		t.Fatalf("after return: %+v", res)
	}
}

func TestBuildModemProbeCheckRunsAfterAFailedRestore(t *testing.T) {
	t.Parallel()

	clk := &sundayClock{now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	set, _ := buildModemProbeCheck(testModemProbeConfig(), &fakeSMS{}, lookupPhone, clk.Now)

	if err := set.Persisters[0].Restore(store.State{KV: map[string]string{modemProbeStateKey: "{"}}); err == nil {
		t.Fatal("a corrupt state must be reported")
	}
	if res := set.Checks[0].Run(t.Context()); res.Err != nil {
		t.Fatalf("the check must start afresh: %+v", res)
	}
}

func TestBuildModemProbeCheckBadTimeIsAStartError(t *testing.T) {
	t.Parallel()

	mp := testModemProbeConfig()
	mp.Time = "noon"
	set, probe := buildModemProbeCheck(mp, &fakeSMS{}, lookupPhone, time.Now)
	if probe != nil || len(set.Checks) != 0 || len(set.Errors) != 1 ||
		set.Errors[0].Key != "modem_probe/config/invalid" {
		t.Fatalf("set = %+v, probe = %v", set, probe)
	}
}

func TestProbeMatcherDefersTheModemUntilRestored(t *testing.T) {
	t.Parallel()

	set, matcher := buildModemProbeCheck(testModemProbeConfig(), &fakeSMS{}, lookupPhone, time.Now)
	if !matcher.Defer(testModemPhone) {
		t.Fatal("before the state is restored a message from the modem must wait")
	}
	if matcher.Defer("+10000000001") {
		t.Fatal("other senders are never deferred")
	}

	if err := set.Persisters[0].Restore(store.State{}); err != nil {
		t.Fatal(err)
	}
	if matcher.Defer(testModemPhone) {
		t.Fatal("once restored nothing is deferred")
	}
}

// fakeInbox is an sms.Inbox serving the rows not yet marked processed.
type fakeInbox struct {
	rows   []sms.InboxMessage
	marked []int64
}

func (f *fakeInbox) Unprocessed(context.Context) ([]sms.InboxMessage, error) {
	var out []sms.InboxMessage
	for _, r := range f.rows {
		if !slices.Contains(f.marked, r.ID) {
			out = append(out, r)
		}
	}

	return out, nil
}

func (f *fakeInbox) MarkProcessed(_ context.Context, ids []int64) error {
	f.marked = append(f.marked, ids...)

	return nil
}

// TestInboxWorkerConsumesProbeReplies wires the daemon's providers: the probe
// built by the health check providers is the inbox worker's matcher.
func TestInboxWorkerConsumesProbeReplies(t *testing.T) {
	t.Setenv("TGSHD_TEST_MODEM_PHONE", testModemPhone)

	db, err := sql.Open("pgx", testGammuDSN) // lazy: never connects here
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mp := testModemProbeConfig()
	mp.PhoneEnv = "TGSHD_TEST_MODEM_PHONE"
	pub := &fakePublisher{}
	inbox := &fakeInbox{}

	type collected struct {
		fx.In

		Workers []app.Worker `group:"worker"`
		Sets    []checkSet   `group:"health_checks"`
	}
	var got collected
	fxApp := fx.New(
		fx.NopLogger,
		fx.Supply(
			&config{GammuDSN: testGammuDSN, ChatID: 9},
			&healthSetup{Config: &hcfg.Config{ModemProbe: mp}},
			zap.NewNop(), db,
			fx.Annotate(&fakeSMS{}, fx.As(new(command.SmsSender))),
			fx.Annotate(pub, fx.As(new(sms.Publisher))),
		),
		healthCheckProviders(),
		gammuInboxProviders(),
		fx.Decorate(func(sms.Inbox) sms.Inbox { return inbox }),
		fx.Invoke(func(c collected) { got = c }),
	)
	if err = fxApp.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got.Workers) != 1 || got.Workers[0].Name() != "sms-inbox" {
		t.Fatalf("workers = %v", got.Workers)
	}
	set := mergeCheckSets(got.Sets)
	if len(set.Checks) != 1 || len(set.Persisters) != 1 {
		t.Fatalf("set = %+v", set)
	}

	// The reply is polled before the stored state is restored: it waits.
	sent := time.Now().Add(-time.Minute)
	inbox.rows = []sms.InboxMessage{
		{ID: 1, SenderNumber: testModemPhone, TextDecoded: "tgshd probe 0123456789ab", ReceivingDateTime: sent},
		{ID: 2, SenderNumber: "+10000000001", TextDecoded: "hello", ReceivingDateTime: sent},
	}
	if err = got.Workers[0].Do(t.Context()); err != nil {
		t.Fatal(err)
	}
	if msgs := pub.messages(); len(msgs) != 1 || !strings.Contains(msgs[0], "hello") {
		t.Fatalf("forwarded = %q, want only the foreign message", msgs)
	}
	if !slices.Equal(inbox.marked, []int64{2}) {
		t.Fatalf("marked = %v, want the probe reply left unprocessed", inbox.marked)
	}

	// An outstanding probe, as stored by the daemon before a restart.
	stored := probeStore(t, modemprobe.State{SentAt: sent, Token: "0123456789ab", FirstMissingAt: sent})
	if err = set.Persisters[0].Restore(stored); err != nil {
		t.Fatal(err)
	}
	if err = got.Workers[0].Do(t.Context()); err != nil {
		t.Fatal(err)
	}
	if msgs := pub.messages(); len(msgs) != 1 {
		t.Fatalf("forwarded = %q, the probe reply must be consumed", msgs)
	}
	if !slices.Equal(inbox.marked, []int64{2, 1}) {
		t.Fatalf("marked = %v", inbox.marked)
	}

	// The matcher recorded the return; the saved state proves it without a
	// clock-dependent run.
	var saved store.State
	if err = set.Persisters[0].Save(&saved); err != nil {
		t.Fatal(err)
	}
	var st modemprobe.State
	if err = json.Unmarshal([]byte(saved.KV[modemProbeStateKey]), &st); err != nil {
		t.Fatal(err)
	}
	if st.Token != "" || st.LastOKAt.IsZero() {
		t.Fatalf("probe state after its reply: %+v", st)
	}
}

func TestInboxWorkerWithoutHealthHasNoMatcher(t *testing.T) {
	t.Parallel()

	inbox := &fakeInbox{rows: []sms.InboxMessage{
		{ID: 1, SenderNumber: testModemPhone, TextDecoded: "tgshd probe 0123456789ab"},
	}}
	pub := &fakePublisher{}
	w := newInboxWorker(inboxParams{Logger: zap.NewNop(), Publisher: pub, Config: &config{ChatID: 9}, Inbox: inbox})
	if err := w.Do(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(pub.messages()) != 1 {
		t.Fatalf("without a probe every SMS is forwarded: %q", pub.messages())
	}
}

func TestAppOptionsWireTheInboxAndProbe(t *testing.T) {
	t.Parallel()

	needsInbox := fx.Invoke(func(sms.Inbox) {})
	needsMatcher := fx.Invoke(func(sms.ProbeMatcher) {})
	tests := []struct {
		name                     string
		env                      map[string]string
		wantInbox, wantMatcherOK bool
	}{
		{name: "neither", env: nil},
		{name: "gammu", env: map[string]string{"BOT_GAMMU_DSN": testGammuDSN}, wantInbox: true},
		{name: "health", env: map[string]string{"BOT_HEALTH_CONFIG": "/etc/tgshd/health.yaml"}, wantMatcherOK: true},
		{name: "gammu and health", env: map[string]string{
			"BOT_GAMMU_DSN": testGammuDSN, "BOT_HEALTH_CONFIG": "/etc/tgshd/health.yaml",
		}, wantInbox: true, wantMatcherOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			getenv := func(k string) string { return tt.env[k] }
			if err := fx.ValidateApp(appOptions(getenv)...); err != nil {
				t.Fatalf("fx graph: %v", err)
			}
			err := fx.ValidateApp(append(appOptions(getenv), needsInbox)...)
			if (err == nil) != tt.wantInbox {
				t.Fatalf("inbox worker wired = %v, want %v (%v)", err == nil, tt.wantInbox, err)
			}
			err = fx.ValidateApp(append(appOptions(getenv), needsMatcher)...)
			if (err == nil) != tt.wantMatcherOK {
				t.Fatalf("probe matcher provided = %v, want %v (%v)", err == nil, tt.wantMatcherOK, err)
			}
		})
	}
}

func TestCheckGraphHasNoSMSSender(t *testing.T) {
	t.Parallel()

	var sets []checkSet
	opts := checkGraph(&config{GammuDSN: testGammuDSN}, &healthSetup{}, zap.NewNop(), healthCheckProviders(), &sets)
	if err := fx.ValidateApp(append(opts, fx.Invoke(func(command.SmsSender) {}))...); err == nil {
		t.Fatal("check --once must never hold an SMS sender")
	}
}

func writeProbeState(t *testing.T, stateDir string, st modemprobe.State) {
	t.Helper()

	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Checkpoint(t.Context(), probeStore(t, st), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
}

const probeSection = "modem_probe:\n  phone_env: TGSHD_TEST_CLI_MODEM_PHONE\n"

func TestRunCheckOnceNeverSendsADueModemProbe(t *testing.T) {
	t.Setenv("TGSHD_TEST_CLI_MODEM_PHONE", testModemPhone)

	path, stateDir := writeHealthConfig(t, probeSection)
	// Never returned, first run a week ago: a probe is due and not yet silent for 8 days.
	writeProbeState(t, stateDir, modemprobe.State{FirstMissingAt: time.Now().Add(-7*24*time.Hour - time.Hour)})

	cio, stdout, stderr := testIO(nil, time.Now())
	if code := runCheckOnce(t.Context(), &config{HealthConfig: path, GammuDSN: testGammuDSN}, cio); code != exitOK {
		t.Fatalf("code = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	out := stdout.String()
	if !strings.Contains(out, "UNKNOWN modem_probe/roundtrip: A modem probe has been due since ") ||
		!strings.Contains(out, "has not sent it") || !strings.Contains(out, "SUMMARY 1 checks") {
		t.Fatalf("stdout = %q", out)
	}
}

func TestRunCheckOnceModemProbeWithoutGammu(t *testing.T) {
	t.Parallel()

	path, _ := writeHealthConfig(t, probeSection)
	cio, stdout, _ := testIO(nil, time.Now())
	if code := runCheckOnce(t.Context(), &config{HealthConfig: path}, cio); code != exitError {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(stdout.String(), "ERROR modem_probe/config/missing: modem_probe requires BOT_GAMMU_DSN") ||
		!strings.Contains(stdout.String(), "SUMMARY 0 checks") {
		t.Fatalf("stdout = %q", stdout)
	}
}
