package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/notify"
	"github.com/Toshik1978/tgshd/pkg/telegram/route"
)

const (
	// rateMarksKey is the reserved kv key holding the JSON-encoded rate marks.
	rateMarksKey = "rate_marks"

	// MaxPendingSamples bounds the samples held in memory while they cannot
	// be written; the oldest are dropped first. A tick yields a few dozen
	// samples at most, so this keeps well over a day of history in a few
	// megabytes; history is a trend aid and losing its oldest part during a
	// long outage is acceptable.
	MaxPendingSamples = 100_000
	// MaxPendingAlerts bounds the alert-log records held in memory while they
	// cannot be written; the oldest are dropped first. Records come from state
	// changes and rate-limited checker errors, so the bound is generous and
	// only reached by a flapping check during a very long outage.
	MaxPendingAlerts = 10_000
)

// ErrDegraded is returned by Checkpoint, once per outage, when state cannot be
// written. The store keeps accepting data in memory and flushes it on the first
// successful Checkpoint afterwards (FR-013).
var ErrDegraded = errors.New("state store write failed, continuing in memory")

// Baseline is the stored per-drive baseline. Data is opaque JSON owned by the
// disks check (counters, self-test fields).
type Baseline struct {
	Serial      string
	Model       string
	Bus         string
	Data        json.RawMessage
	FirstSeenAt time.Time
	LastSeenAt  time.Time
	Retired     bool
}

// State is the in-memory state the store checkpoints and restores.
//
// Conditions, RateMarks and Queue are full snapshots: Checkpoint replaces the
// stored rows wholesale, so anything absent was resolved, expired or sent.
// Baselines (by serial) and KV (by key) are upserts and are never deleted by a
// checkpoint that omits them. RateMarks live in kv under a reserved key, which
// Load never exposes in KV.
type State struct {
	Conditions []health.Condition
	Baselines  []Baseline
	KV         map[string]string
	RateMarks  []health.RateMark
	Queue      []notify.QueuedMessage
}

// AlertKind is the kind of an alert log record; the schema restricts it to the
// values below.
type AlertKind string

// Alert kinds accepted by the alerts table.
const (
	KindProblem      AlertKind = "problem"
	KindResolved     AlertKind = "resolved"
	KindEvent        AlertKind = "event"
	KindFix          AlertKind = "fix"
	KindRetired      AlertKind = "retired"
	KindCleared      AlertKind = "cleared"
	KindCheckerError AlertKind = "checker_error"
)

// Alert is one append-only alert log record.
type Alert struct {
	At    time.Time
	Kind  AlertKind
	Check string
	Key   string
	Text  string
}

// SampleRow is one append-only numeric history record.
type SampleRow struct {
	At     time.Time
	Metric string
	Value  float64
}

// checkpointer holds what could not be written yet while the store is degraded.
type checkpointer struct {
	mu       sync.Mutex
	degraded bool
	state    State
	alerts   []Alert
	samples  []SampleRow
	// maxAlerts and maxSamples bound alerts and samples; zero means
	// MaxPendingAlerts and MaxPendingSamples.
	maxAlerts, maxSamples int
	// droppedAlerts and droppedSamples count the records dropped by the bounds.
	droppedAlerts, droppedSamples int
}

// Load reads the persisted state.
func (s *Store) Load(ctx context.Context) (State, error) {
	var st State
	var err error
	if st.Conditions, err = s.loadConditions(ctx); err != nil {
		return State{}, err
	}
	if st.Baselines, err = s.loadBaselines(ctx); err != nil {
		return State{}, err
	}
	if st.KV, err = s.loadKV(ctx); err != nil {
		return State{}, err
	}
	if raw, ok := st.KV[rateMarksKey]; ok {
		delete(st.KV, rateMarksKey)
		st.RateMarks = decodeRateMarks(raw)
	}
	if st.Queue, err = s.loadQueue(ctx); err != nil {
		return State{}, err
	}

	return st, nil
}

// Checkpoint persists dirty and appends newAlerts and newSamples in one
// transaction. dirty carries the latest full snapshot (see State).
//
// On a write failure nothing is committed, the data is retained in memory and
// ErrDegraded (wrapping the cause) is returned on the first failure only. Every
// later call retries, with the pending alerts and samples accumulated (the
// oldest dropped beyond MaxPendingAlerts and MaxPendingSamples) and the latest
// snapshot replacing earlier ones; the first success flushes it all. A
// cancelled ctx writes nothing but retains the data the same way.
func (s *Store) Checkpoint(ctx context.Context, dirty State, newAlerts []Alert, newSamples []SampleRow) error {
	if err := validate(dirty, newAlerts); err != nil {
		return err
	}
	c := &s.ckpt
	c.mu.Lock()
	defer c.mu.Unlock()

	c.merge(dirty)
	c.add(newAlerts, newSamples)
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}

	if err := s.write(ctx, c.state, c.alerts, c.samples); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("checkpoint: %w", ctx.Err())
		}
		if c.degraded {
			return nil
		}
		c.degraded = true

		return fmt.Errorf("%w: %w", ErrDegraded, err)
	}
	c.degraded = false
	c.state, c.alerts, c.samples = State{}, nil, nil

	return nil
}

// Degraded reports whether the last checkpoint could not be written.
func (s *Store) Degraded() bool {
	s.ckpt.mu.Lock()
	defer s.ckpt.mu.Unlock()

	return s.ckpt.degraded
}

// Dropped returns how many pending alerts and samples were dropped, oldest
// first, because they could not be written before their bound was reached.
func (s *Store) Dropped() (alerts, samples int) {
	s.ckpt.mu.Lock()
	defer s.ckpt.mu.Unlock()

	return s.ckpt.droppedAlerts, s.ckpt.droppedSamples
}

// add appends the new records to the pending ones, keeping the newest within
// the bounds.
func (c *checkpointer) add(alerts []Alert, samples []SampleRow) {
	var n int
	c.alerts, n = KeepNewest(append(c.alerts, alerts...), cmp.Or(c.maxAlerts, MaxPendingAlerts))
	c.droppedAlerts += n
	c.samples, n = KeepNewest(append(c.samples, samples...), cmp.Or(c.maxSamples, MaxPendingSamples))
	c.droppedSamples += n
}

// KeepNewest returns the last limit elements of s, moved to the front of its
// backing array, and how many were dropped from its front.
func KeepNewest[T any](s []T, limit int) (kept []T, dropped int) {
	if len(s) <= limit {
		return s, 0
	}
	dropped = len(s) - limit

	return append(s[:0], s[dropped:]...), dropped
}

// validate rejects values the schema would refuse, so one bad record cannot
// wedge the pending buffers.
func validate(st State, alerts []Alert) error {
	for i := range alerts {
		switch alerts[i].Kind {
		case KindProblem, KindResolved, KindEvent, KindFix, KindRetired, KindCleared, KindCheckerError:
		default:
			return fmt.Errorf("invalid alert kind %q", alerts[i].Kind)
		}
	}
	for i := range st.Queue {
		if err := validateQueued(&st.Queue[i]); err != nil {
			return err
		}
	}

	return nil
}

func validateQueued(m *notify.QueuedMessage) error {
	switch m.Channel {
	case "tlg", "sms", "email":
	default:
		return fmt.Errorf("invalid queue channel %q", m.Channel)
	}
	switch m.Kind {
	case "problem", "resolved", "event":
	default:
		return fmt.Errorf("invalid queue kind %q", m.Kind)
	}
	if m.Route != (route.Route{}) && m.Route.String() == "" {
		return fmt.Errorf("invalid queue route %+v", m.Route)
	}

	return nil
}

// merge folds a newer snapshot into the pending one: snapshots replace,
// upserts accumulate with the newer value winning.
func (c *checkpointer) merge(dirty State) {
	c.state.Conditions = slices.Clone(dirty.Conditions)
	c.state.RateMarks = slices.Clone(dirty.RateMarks)
	c.state.Queue = slices.Clone(dirty.Queue)

	byBase := map[string]int{}
	for i := range c.state.Baselines {
		byBase[c.state.Baselines[i].Serial] = i
	}
	for i := range dirty.Baselines {
		b := &dirty.Baselines[i]
		if j, ok := byBase[b.Serial]; ok {
			c.state.Baselines[j] = *b
			continue
		}
		byBase[b.Serial] = len(c.state.Baselines)
		c.state.Baselines = append(c.state.Baselines, *b)
	}

	if len(dirty.KV) > 0 && c.state.KV == nil {
		c.state.KV = map[string]string{}
	}
	maps.Copy(c.state.KV, dirty.KV)
}

func (s *Store) write(ctx context.Context, st State, alerts []Alert, samples []SampleRow) error {
	marks, err := json.Marshal(encodeRateMarks(st.RateMarks))
	if err != nil {
		return fmt.Errorf("encode rate marks: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin checkpoint: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	steps := []func() error{
		func() error { return writeConditions(ctx, tx, st.Conditions) },
		func() error { return writeBaselines(ctx, tx, st.Baselines) },
		func() error { return writeKV(ctx, tx, st.KV, string(marks)) },
		func() error { return writeQueue(ctx, tx, st.Queue) },
		func() error { return writeAlerts(ctx, tx, alerts) },
		func() error { return writeSamples(ctx, tx, samples) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit checkpoint: %w", err)
	}

	return nil
}

func writeConditions(ctx context.Context, tx *sql.Tx, conds []health.Condition) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM conditions`); err != nil {
		return fmt.Errorf("clear conditions: %w", err)
	}
	for i := range conds {
		c := &conds[i]
		_, err := tx.ExecContext(ctx, `INSERT INTO conditions
			(key, check_name, streak_start, bad_for_s, last_reading_at, last_text, open, opened_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			c.Key, c.Check, nullUnix(c.StreakStart), int64(c.BadFor/time.Second),
			nullUnix(c.LastReadingAt), c.LastText, c.Open, nullUnix(c.OpenedAt))
		if err != nil {
			return fmt.Errorf("write condition %q: %w", c.Key, err)
		}
	}

	return nil
}

func writeBaselines(ctx context.Context, tx *sql.Tx, bases []Baseline) error {
	for i := range bases {
		b := &bases[i]
		data := string(b.Data)
		if data == "" {
			data = "{}"
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO baselines
			(serial, model, bus, data, first_seen_at, last_seen_at, retired)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(serial) DO UPDATE SET model = excluded.model, bus = excluded.bus,
			data = excluded.data, first_seen_at = excluded.first_seen_at,
			last_seen_at = excluded.last_seen_at, retired = excluded.retired`,
			b.Serial, b.Model, b.Bus, data, b.FirstSeenAt.Unix(), b.LastSeenAt.Unix(), b.Retired)
		if err != nil {
			return fmt.Errorf("write baseline: %w", err)
		}
	}

	return nil
}

// PutKV writes one kv value at once, outside the checkpoint, and touches
// nothing else: the memory guard writes its intervention records as soon as
// an action completes, and owns their key. The reserved rate-marks key is
// refused.
func (s *Store) PutKV(ctx context.Context, key, value string) error {
	if key == rateMarksKey {
		return fmt.Errorf("put kv %q: the key is reserved", key)
	}
	if _, err := s.db.ExecContext(ctx, kvUpsert, key, value); err != nil {
		return fmt.Errorf("put kv %q: %w", key, err)
	}

	return nil
}

// kvUpsert writes one kv value.
const kvUpsert = `INSERT INTO kv (k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`

func writeKV(ctx context.Context, tx *sql.Tx, kv map[string]string, marks string) error {
	for k, v := range kv {
		if k == rateMarksKey {
			continue
		}
		if _, err := tx.ExecContext(ctx, kvUpsert, k, v); err != nil {
			return fmt.Errorf("write kv %q: %w", k, err)
		}
	}
	if _, err := tx.ExecContext(ctx, kvUpsert, rateMarksKey, marks); err != nil {
		return fmt.Errorf("write rate marks: %w", err)
	}

	return nil
}

func writeQueue(ctx context.Context, tx *sql.Tx, queue []notify.QueuedMessage) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM queue`); err != nil {
		return fmt.Errorf("clear queue: %w", err)
	}
	for i := range queue {
		m := &queue[i]
		_, err := tx.ExecContext(ctx,
			`INSERT INTO queue (id, created_at, channel, text, attempts, kind, check_name, key, plain, since, route)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ID, m.CreatedAt.Unix(), m.Channel, m.Text, m.Attempts, m.Kind, m.Check, m.Key,
			m.Plain, nullUnix(m.Since), m.Route.String())
		if err != nil {
			return fmt.Errorf("write queue message %d: %w", m.ID, err)
		}
	}

	return nil
}

func writeAlerts(ctx context.Context, tx *sql.Tx, alerts []Alert) error {
	for _, a := range alerts {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO alerts (at, kind, check_name, key, text) VALUES (?, ?, ?, ?, ?)`,
			a.At.Unix(), string(a.Kind), a.Check, a.Key, a.Text)
		if err != nil {
			return fmt.Errorf("write alert: %w", err)
		}
	}

	return nil
}

func writeSamples(ctx context.Context, tx *sql.Tx, samples []SampleRow) error {
	for _, r := range samples {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO samples (at, metric, value) VALUES (?, ?, ?)`, r.At.Unix(), r.Metric, r.Value)
		if err != nil {
			return fmt.Errorf("write sample: %w", err)
		}
	}

	return nil
}

func (s *Store) loadConditions(ctx context.Context) ([]health.Condition, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, check_name, streak_start, bad_for_s,
		last_reading_at, last_text, open, opened_at FROM conditions ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("load conditions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []health.Condition
	for rows.Next() {
		var c health.Condition
		var streak, last, opened sql.NullInt64
		var badFor int64
		if err := rows.Scan(&c.Key, &c.Check, &streak, &badFor, &last, &c.LastText, &c.Open, &opened); err != nil {
			return nil, fmt.Errorf("scan condition: %w", err)
		}
		c.BadFor = time.Duration(badFor) * time.Second
		c.StreakStart, c.LastReadingAt, c.OpenedAt = fromNull(streak), fromNull(last), fromNull(opened)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load conditions: %w", err)
	}

	return out, nil
}

func (s *Store) loadBaselines(ctx context.Context) ([]Baseline, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT serial, COALESCE(model, ''), COALESCE(bus, ''), data,
		first_seen_at, last_seen_at, retired FROM baselines ORDER BY serial`)
	if err != nil {
		return nil, fmt.Errorf("load baselines: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Baseline
	for rows.Next() {
		var b Baseline
		var data string
		var first, last int64
		if err := rows.Scan(&b.Serial, &b.Model, &b.Bus, &data, &first, &last, &b.Retired); err != nil {
			return nil, fmt.Errorf("scan baseline: %w", err)
		}
		b.Data = json.RawMessage(data)
		b.FirstSeenAt, b.LastSeenAt = time.Unix(first, 0).UTC(), time.Unix(last, 0).UTC()
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load baselines: %w", err)
	}

	return out, nil
}

func (s *Store) loadKV(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT k, v FROM kv`)
	if err != nil {
		return nil, fmt.Errorf("load kv: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scan kv: %w", err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load kv: %w", err)
	}

	return out, nil
}

func (s *Store) loadQueue(ctx context.Context) ([]notify.QueuedMessage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, created_at, channel, text, attempts, kind, check_name, key, plain, since, route
		FROM queue ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("load queue: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []notify.QueuedMessage
	for rows.Next() {
		var m notify.QueuedMessage
		var created int64
		var since sql.NullInt64
		var routeText string
		err := rows.Scan(&m.ID, &created, &m.Channel, &m.Text, &m.Attempts, &m.Kind, &m.Check, &m.Key,
			&m.Plain, &since, &routeText)
		if err != nil {
			return nil, fmt.Errorf("scan queue message: %w", err)
		}
		if m.Route, err = route.ParseRoute(routeText); err != nil {
			return nil, fmt.Errorf("queue message %d: %w", m.ID, err)
		}
		m.CreatedAt = time.Unix(created, 0).UTC()
		m.Since = fromNull(since)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load queue: %w", err)
	}

	return out, nil
}

type rateMarkJSON struct {
	Key     string `json:"key"`
	Channel string `json:"channel"`
	At      int64  `json:"at"`
}

func encodeRateMarks(marks []health.RateMark) []rateMarkJSON {
	out := make([]rateMarkJSON, 0, len(marks))
	for _, m := range marks {
		out = append(out, rateMarkJSON{Key: m.Key, Channel: m.Channel, At: m.LastSentAt.Unix()})
	}

	return out
}

// decodeRateMarks returns no marks for a corrupt value: rate marks only
// suppress repeats, so losing them is harmless and must not block startup.
func decodeRateMarks(raw string) []health.RateMark {
	var in []rateMarkJSON
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return nil
	}
	var out []health.RateMark
	for _, m := range in {
		out = append(out, health.RateMark{Key: m.Key, Channel: m.Channel, LastSentAt: time.Unix(m.At, 0).UTC()})
	}

	return out
}

// nullUnix stores the zero time as NULL and anything else as Unix seconds.
func nullUnix(t time.Time) any {
	if t.IsZero() {
		return nil
	}

	return t.Unix()
}

func fromNull(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}

	return time.Unix(n.Int64, 0).UTC()
}
