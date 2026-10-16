package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health/notify"
)

// Retention windows (data-model "Retention").
const (
	alertRetention  = 365 * 24 * time.Hour
	sampleRetention = 90 * 24 * time.Hour
	queueRetention  = 7 * 24 * time.Hour
	statsWindow     = 7 * 24 * time.Hour
)

// MetricStats is the current value and the 7-day aggregate of one metric.
type MetricStats struct {
	Metric string
	// Current is the latest raw sample; HasCurrent is false when only rolled-up
	// daily rows exist for the metric.
	Current    float64
	CurrentAt  time.Time
	HasCurrent bool
	// Min, Avg and Max cover the 7 days before now, combining raw samples and
	// samples_daily rows (Avg is weighted by sample count); N is the sample count.
	Min, Avg, Max float64
	N             int
}

// RetentionResult reports what one Retention run did.
type RetentionResult struct {
	AlertsDeleted int
	// SamplesRolledUp is the number of raw samples aggregated and deleted;
	// DaysWritten the number of samples_daily rows inserted or merged.
	SamplesRolledUp int
	DaysWritten     int
	// QueueDropped are the undelivered messages older than 7 days that were
	// deleted; the caller logs them.
	QueueDropped []notify.QueuedMessage
}

// Alerts returns alert log records at or after since, newest first, at most
// limit (limit <= 0 means no limit). A non-empty filter keeps records whose
// check name equals it or whose condition key starts with it. It works on a
// store opened with OpenReadOnly.
func (s *Store) Alerts(ctx context.Context, filter string, since time.Time, limit int) ([]Alert, error) {
	query := `SELECT at, kind, check_name, key, text FROM alerts WHERE at >= ?`
	args := []any{since.Unix()}
	if filter != "" {
		query += ` AND (check_name = ? OR substr(key, 1, ?) = ?)`
		args = append(args, filter, len(filter), filter)
	}
	query += ` ORDER BY at DESC, id DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query alerts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Alert
	for rows.Next() {
		var a Alert
		var at int64
		var kind string
		if err := rows.Scan(&at, &kind, &a.Check, &a.Key, &a.Text); err != nil {
			return nil, fmt.Errorf("scan alert: %w", err)
		}
		a.At, a.Kind = time.Unix(at, 0).UTC(), AlertKind(kind)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query alerts: %w", err)
	}

	return out, nil
}

// Metrics returns, per metric whose name starts with prefix (empty means all)
// and that has data in the 7 days before now, the current value and the 7-day
// min/avg/max, ordered by metric name. Daily rows count from the local day
// containing the window start (in now's location). It works on a store opened
// with OpenReadOnly.
func (s *Store) Metrics(ctx context.Context, prefix string, now time.Time) ([]MetricStats, error) {
	from := now.Add(-statsWindow)
	const agg = `
WITH u AS (
  SELECT metric, value AS mn, value AS av, value AS mx, 1 AS n FROM samples
    WHERE at >= ?1 AND at <= ?2 AND substr(metric, 1, ?3) = ?4
  UNION ALL
  SELECT metric, min, avg, max, n FROM samples_daily
    WHERE day >= ?5 AND day <= ?2 AND substr(metric, 1, ?3) = ?4
)
SELECT metric, MIN(mn), SUM(av * n) / SUM(n), MAX(mx), SUM(n) FROM u GROUP BY metric ORDER BY metric`
	out, err := s.queryStats(ctx, agg, from.Unix(), now.Unix(), len(prefix), prefix, dayStart(from))
	if err != nil {
		return nil, err
	}

	for i := range out {
		var at int64
		err := s.db.QueryRowContext(ctx,
			`SELECT at, value FROM samples WHERE metric = ? AND at <= ? ORDER BY at DESC LIMIT 1`,
			out[i].Metric, now.Unix()).Scan(&at, &out[i].Current)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return nil, fmt.Errorf("query current %q: %w", out[i].Metric, err)
		default:
			out[i].CurrentAt, out[i].HasCurrent = time.Unix(at, 0).UTC(), true
		}
	}

	return out, nil
}

// queryStats runs the aggregate query and closes its rows, releasing the single
// connection before the caller issues further queries.
func (s *Store) queryStats(ctx context.Context, query string, args ...any) ([]MetricStats, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query metrics: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []MetricStats
	for rows.Next() {
		var m MetricStats
		if err := rows.Scan(&m.Metric, &m.Min, &m.Avg, &m.Max, &m.N); err != nil {
			return nil, fmt.Errorf("scan metric: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query metrics: %w", err)
	}

	return out, nil
}

// Retention applies the daily retention rules in one transaction: alerts older
// than 365 days are deleted; samples older than 90 days are aggregated into
// samples_daily (merged with any existing row for the same local day and metric)
// and deleted; queue entries older than 7 days are deleted and returned. Local
// days are taken in now's location. It needs a writable store; the runner's
// daily job calls it and logs RetentionResult.QueueDropped.
func (s *Store) Retention(ctx context.Context, now time.Time) (RetentionResult, error) {
	var res RetentionResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("begin retention: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	r, err := tx.ExecContext(ctx, `DELETE FROM alerts WHERE at < ?`, now.Add(-alertRetention).Unix())
	if err != nil {
		return res, fmt.Errorf("delete old alerts: %w", err)
	}
	n, _ := r.RowsAffected()
	res.AlertsDeleted = int(n)

	cutoff := now.Add(-sampleRetention).Unix()
	if res.SamplesRolledUp, res.DaysWritten, err = rollUpSamples(ctx, tx, cutoff, now.Location()); err != nil {
		return RetentionResult{}, err
	}
	if res.QueueDropped, err = dropOldQueue(ctx, tx, now.Add(-queueRetention).Unix()); err != nil {
		return RetentionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return RetentionResult{}, fmt.Errorf("commit retention: %w", err)
	}

	return res, nil
}

type dayKey struct {
	day    int64
	metric string
}

type dayAgg struct {
	min, max, sum float64
	n             int
}

func rollUpSamples(ctx context.Context, tx *sql.Tx, cutoff int64, loc *time.Location) (rolled, days int, err error) {
	aggs, rolled, err := aggregateOldSamples(ctx, tx, cutoff, loc)
	if err != nil {
		return 0, 0, err
	}
	for k, a := range aggs {
		// Merge with an existing row: min of mins, max of maxes, weighted avg, n sum.
		_, err := tx.ExecContext(ctx, `INSERT INTO samples_daily (day, metric, min, avg, max, n)
			VALUES (?1, ?2, ?3, ?4, ?5, ?6)
			ON CONFLICT(day, metric) DO UPDATE SET
			  min = MIN(min, excluded.min), max = MAX(max, excluded.max),
			  avg = (avg * n + excluded.avg * excluded.n) / (n + excluded.n),
			  n = n + excluded.n`,
			k.day, k.metric, a.min, a.sum/float64(a.n), a.max, a.n)
		if err != nil {
			return 0, 0, fmt.Errorf("upsert daily %q: %w", k.metric, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM samples WHERE at < ?`, cutoff); err != nil {
		return 0, 0, fmt.Errorf("delete old samples: %w", err)
	}

	return rolled, len(aggs), nil
}

func aggregateOldSamples(
	ctx context.Context, tx *sql.Tx, cutoff int64, loc *time.Location,
) (map[dayKey]*dayAgg, int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT at, metric, value FROM samples WHERE at < ?`, cutoff)
	if err != nil {
		return nil, 0, fmt.Errorf("read old samples: %w", err)
	}
	defer func() { _ = rows.Close() }()

	aggs := map[dayKey]*dayAgg{}
	total := 0
	for rows.Next() {
		var at int64
		var metric string
		var v float64
		if err := rows.Scan(&at, &metric, &v); err != nil {
			return nil, 0, fmt.Errorf("scan old sample: %w", err)
		}
		k := dayKey{day: dayStart(time.Unix(at, 0).In(loc)), metric: metric}
		a := aggs[k]
		if a == nil {
			a = &dayAgg{min: v, max: v}
			aggs[k] = a
		}
		a.min, a.max = min(a.min, v), max(a.max, v)
		a.sum += v
		a.n++
		total++
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read old samples: %w", err)
	}

	return aggs, total, nil
}

func dropOldQueue(ctx context.Context, tx *sql.Tx, cutoff int64) ([]notify.QueuedMessage, error) {
	out, err := readOldQueue(ctx, tx, cutoff)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM queue WHERE created_at < ?`, cutoff); err != nil {
		return nil, fmt.Errorf("delete old queue: %w", err)
	}

	return out, nil
}

func readOldQueue(ctx context.Context, tx *sql.Tx, cutoff int64) ([]notify.QueuedMessage, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, created_at, channel, text, attempts FROM queue WHERE created_at < ? ORDER BY id`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("read old queue: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []notify.QueuedMessage
	for rows.Next() {
		var m notify.QueuedMessage
		var created int64
		if err := rows.Scan(&m.ID, &created, &m.Channel, &m.Text, &m.Attempts); err != nil {
			return nil, fmt.Errorf("scan old queue message: %w", err)
		}
		m.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read old queue: %w", err)
	}

	return out, nil
}

// dayStart returns the Unix time of 00:00 in t's location on t's calendar day.
func dayStart(t time.Time) int64 {
	y, m, d := t.Date()

	return time.Date(y, m, d, 0, 0, 0, 0, t.Location()).Unix()
}
