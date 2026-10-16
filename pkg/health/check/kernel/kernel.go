// Package kernel implements the kernel hardware-error check: error classes
// recognised in /dev/kmsg records plus rising EDAC memory-controller counters.
//
// Records are fed by a long-lived reader goroutine through Consume; Run, called
// on every interval, turns what accumulated into Event readings.
package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
)

const (
	// stillWindow is the minimum gap between "still happening" messages.
	stillWindow = 6 * time.Hour
	// quietAfter is the silence after which an episode ends.
	quietAfter = 24 * time.Hour
	// sourceTimeout bounds every call into a source adapter.
	sourceTimeout = 10 * time.Second
	// maxSample caps the sample line quoted in a message.
	maxSample = 200
	// maxInbox caps the matching records buffered between two Runs.
	maxInbox = 10000

	channel = "tlg"
)

// Record is one kernel log record, as delivered by the kmsg reader. Level 0
// is the most severe kernel level and 7 the least.
type Record struct {
	Facility int
	Level    int
	Seq      uint64
	Time     time.Duration // since boot
	Message  string
}

// Counters holds the cumulative error counts of one EDAC memory controller.
type Counters struct {
	Controller    string `json:"controller"`
	Correctable   uint64 `json:"correctable"`
	Uncorrectable uint64 `json:"uncorrectable"`
}

// BootSource reports the current boot id.
type BootSource interface {
	BootID() (string, error)
}

// EDACSource reads the EDAC counters; no EDAC support yields nil, nil.
type EDACSource interface {
	Read() ([]Counters, error)
}

type class struct {
	key      string
	label    string
	maxLevel int // records with a numerically higher (less severe) level are ignored
	re       []*regexp.Regexp
}

// classTable returns the anchored pattern table, one entry per error class.
func classTable() []class {
	compile := func(exprs ...string) []*regexp.Regexp {
		out := make([]*regexp.Regexp, len(exprs))
		for i, e := range exprs {
			out[i] = regexp.MustCompile(e)
		}

		return out
	}

	const anyLevel = 7
	const warning = 4

	return []class{
		{"kernel/storage-io", "storage I/O errors", anyLevel, compile(
			`^blk_update_request: I/O error`,
			`^(I/O|critical (medium|target|nexus)|timeout) error, dev `,
			`^Buffer I/O error on dev `,
		)},
		{
			"kernel/nvme",
			"NVMe errors",
			warning,
			compile(
				`^nvme( nvme)?\d+: I/O .*timeout`,
				`^nvme( nvme)?\d+: .*(resetting controller|controller is down|Device not ready|I/O error)`,
			),
		},
		{"kernel/ata", "ATA errors", warning, compile(
			`^ata\d+(\.\d+)?: (exception|failed command|hard resetting)`,
		)},
		{"kernel/machine-check", "machine check errors", anyLevel, compile(
			`^mce: \[Hardware Error\]`,
		)},
		{"kernel/thermal-throttle", "thermal throttling", anyLevel, compile(
			`^(CPU\d+: )?(Core|Package) temperature above threshold`,
		)},
	}
}

// Episode tracks one ongoing run of records of a class.
type Episode struct {
	FirstAt     time.Time `json:"first_at"`
	LastAt      time.Time `json:"last_at"`
	LastStillAt time.Time `json:"last_still_at"`
	Count       int       `json:"count"`
}

// Cursor is the position in the kernel log that was fully processed.
type Cursor struct {
	BootID string `json:"boot_id"`
	Seq    uint64 `json:"seq"`
	Seen   bool   `json:"seen"` // false until a record was processed in this boot
}

type state struct {
	Cursor   Cursor              `json:"cursor"`
	Episodes map[string]Episode  `json:"episodes"`
	EDAC     map[string]Counters `json:"edac"`
}

// entry is a matching record waiting for the next Run.
type entry struct {
	seq uint64
	key string
	msg string
}

type pending struct {
	count  int
	sample string
}

// Check is the kernel check. It is safe for concurrent use.
type Check struct {
	boot  BootSource
	edac  EDACSource
	marks *health.RateMarks
	now   func() time.Time

	classes []class

	timeout time.Duration

	mu      sync.Mutex
	st      state
	inbox   []entry // matching records consumed since the last Run
	hasSeen bool    // any record consumed in this process
	maxSeq  uint64  // highest Seq consumed, not yet committed to st.Cursor
	bootOK  bool
	bootID  string
	bootErr error
}

// Option customises a Check.
type Option func(*Check)

// WithTimeout bounds every call into a source adapter (default 10 s).
func WithTimeout(d time.Duration) Option {
	return func(c *Check) { c.timeout = d }
}

// New returns a kernel check. now is the clock; marks rate-limits the "still
// happening" messages.
func New(
	boot BootSource, edac EDACSource, marks *health.RateMarks, now func() time.Time, opts ...Option,
) *Check {
	c := &Check{
		boot:    boot,
		edac:    edac,
		marks:   marks,
		now:     now,
		classes: classTable(),
		timeout: sourceTimeout,
		st:      newState(),
	}
	for _, o := range opts {
		o(c)
	}

	return c
}

func newState() state {
	return state{Episodes: make(map[string]Episode), EDAC: make(map[string]Counters)}
}

// Consume records one kernel log record. It never touches the sources: the
// record is only buffered (matching ones) and filtered against the persisted
// cursor by the next Run, which also covers a boot id that is still unreadable.
// More than maxInbox matching records between two Runs are dropped.
func (c *Check) Consume(r Record) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.hasSeen && r.Seq <= c.maxSeq {
		return
	}
	c.hasSeen = true
	c.maxSeq = r.Seq
	if r.Facility != 0 {
		return
	}

	for _, cl := range c.classes {
		if r.Level > cl.maxLevel {
			continue
		}
		for _, re := range cl.re {
			if re.MatchString(r.Message) {
				if len(c.inbox) < maxInbox {
					c.inbox = append(c.inbox, entry{seq: r.Seq, key: cl.key, msg: r.Message})
				}

				return
			}
		}
	}
}

// Run turns the accumulated records and the EDAC counters into readings. When
// the EDAC counters cannot be read, the result carries both the kernel log
// readings and the error.
func (c *Check) Run(ctx context.Context) health.CheckResult {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return health.CheckResult{Err: fmt.Errorf("kernel check: %w", err)}
	}
	c.resolveBoot(ctx)
	if c.bootErr != nil {
		return health.CheckResult{Err: c.bootErr}
	}

	now := c.now()
	readings := c.kernelReadings(now)

	edacReadings, err := c.checkEDAC(ctx)
	readings = append(readings, edacReadings...)

	return health.CheckResult{Readings: readings, Err: err}
}

// Export returns the persistent state (cursor, episodes, EDAC values) as JSON,
// for the store kv. The cursor covers only records already turned into
// readings, so a crash never loses a record.
func (c *Check) Export() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	b, err := json.Marshal(c.st)
	if err != nil {
		return "", fmt.Errorf("encode kernel state: %w", err)
	}

	return string(b), nil
}

// Restore loads state produced by Export. An empty string is a no-op; a
// malformed one is an error and leaves the state untouched.
func (c *Check) Restore(s string) error {
	if s == "" {
		return nil
	}
	st := newState()
	if err := json.Unmarshal([]byte(s), &st); err != nil {
		return fmt.Errorf("decode kernel state: %w", err)
	}
	if st.Episodes == nil {
		st.Episodes = make(map[string]Episode)
	}
	if st.EDAC == nil {
		st.EDAC = make(map[string]Counters)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.st = st
	c.bootOK = false

	return nil
}

// resolveBoot learns the boot id once and resets the cursor on a new boot.
// The caller holds c.mu.
func (c *Check) resolveBoot(ctx context.Context) {
	if c.bootOK {
		return
	}
	var (
		id    string
		idErr error
	)
	err := callTimeout(ctx, c.timeout, func() { id, idErr = c.boot.BootID() })
	if err == nil {
		err = idErr
	}
	if err != nil {
		c.bootErr = fmt.Errorf("read boot id: %w", err)

		return
	}
	c.bootErr = nil
	c.bootOK = true
	c.bootID = id
}

// kernelReadings applies the cursor to the buffered records and turns them into
// readings. With no saved cursor (fresh install) the first drain that has seen
// any record only records the position, announcing nothing: the ring buffer
// replay is history. A saved cursor of another boot resets and announces.
func (c *Check) kernelReadings(now time.Time) []health.Reading {
	inbox := c.inbox
	c.inbox = nil

	if c.st.Cursor.BootID == "" {
		if !c.hasSeen {
			return nil
		}
		c.st.Cursor = Cursor{BootID: c.bootID, Seq: c.maxSeq, Seen: true}

		return nil
	}
	if c.st.Cursor.BootID != c.bootID {
		c.st.Cursor = Cursor{BootID: c.bootID}
	}

	cur := c.st.Cursor
	agg := make(map[string]*pending)
	for _, e := range inbox {
		if cur.Seen && e.seq <= cur.Seq {
			continue
		}
		p := agg[e.key]
		if p == nil {
			p = &pending{}
			agg[e.key] = p
		}
		p.count++
		p.sample = e.msg
	}
	if c.hasSeen {
		c.st.Cursor.Seq = c.maxSeq
		if cur.Seen {
			c.st.Cursor.Seq = max(cur.Seq, c.maxSeq)
		}
		c.st.Cursor.Seen = true
	}

	return c.drainRecords(now, agg)
}

func (c *Check) drainRecords(now time.Time, agg map[string]*pending) []health.Reading {
	var out []health.Reading
	for _, cl := range c.classes {
		p := agg[cl.key]
		if p == nil {
			continue
		}

		ep, ok := c.st.Episodes[cl.key]
		if !ok || now.Sub(ep.LastAt) >= quietAfter {
			c.marks.Allow(cl.key, channel, now, stillWindow)
			c.st.Episodes[cl.key] = Episode{FirstAt: now, LastAt: now, LastStillAt: now, Count: p.count}
			out = append(out, event(cl.key, fmt.Sprintf("Kernel: %s started (%s). Last: %s",
				cl.label, records(p.count), clip(p.sample))))

			continue
		}

		ep.LastAt = now
		ep.Count += p.count
		if now.Sub(ep.LastStillAt) >= stillWindow && c.marks.Allow(cl.key, channel, now, stillWindow) {
			ep.LastStillAt = now
			out = append(out, event(cl.key, fmt.Sprintf("Kernel: %s still happening (%s since %s). Last: %s",
				cl.label, records(ep.Count), ep.FirstAt.UTC().Format(time.RFC3339), clip(p.sample))))
		}
		c.st.Episodes[cl.key] = ep
	}

	return out
}

func (c *Check) checkEDAC(ctx context.Context) ([]health.Reading, error) {
	var (
		ctrls   []Counters
		readErr error
	)
	err := callTimeout(ctx, c.timeout, func() { ctrls, readErr = c.edac.Read() })
	if err == nil {
		err = readErr
	}
	if err != nil {
		return nil, fmt.Errorf("read EDAC counters: %w", err)
	}

	var out []health.Reading
	for _, ctrl := range ctrls {
		prev, known := c.st.EDAC[ctrl.Controller]
		if known {
			if ctrl.Correctable > prev.Correctable {
				out = append(out, event("kernel/edac/"+ctrl.Controller+"/ce", fmt.Sprintf(
					"Kernel: EDAC controller %s corrected memory errors rose from %d to %d",
					ctrl.Controller, prev.Correctable, ctrl.Correctable,
				)))
			}
			if ctrl.Uncorrectable > prev.Uncorrectable {
				out = append(out, event("kernel/edac/"+ctrl.Controller+"/ue", fmt.Sprintf(
					"Kernel: EDAC controller %s uncorrected memory errors rose from %d to %d",
					ctrl.Controller, prev.Uncorrectable, ctrl.Uncorrectable,
				)))
			}
		}
		c.st.EDAC[ctrl.Controller] = ctrl
	}

	return out, nil
}

func event(key, text string) health.Reading {
	return health.Reading{Key: key, Verdict: health.Bad, Text: text, Kind: health.Event}
}

func records(n int) string {
	if n == 1 {
		return "1 record"
	}

	return fmt.Sprintf("%d records", n)
}

func clip(s string) string {
	if len(s) > maxSample {
		return s[:maxSample]
	}

	return s
}

// callTimeout runs fn, giving up when ctx ends or limit elapses.
func callTimeout(ctx context.Context, limit time.Duration, fn func()) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("call cancelled: %w", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()

	timer := time.NewTimer(limit)
	defer timer.Stop()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("call cancelled: %w", ctx.Err())
	case <-timer.C:
		return errors.New("source call timed out")
	}
}
