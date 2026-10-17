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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

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
	// Err is set when this controller could not be read. Its stored counts are kept and
	// it is not judged.
	Err error `json:"-"`
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
	exclude  *regexp.Regexp // a record matching it never belongs to the class; nil for none
}

// classTable returns the pattern table, one entry per error class. The order
// runs from specific to general and the first class that matches a record wins.
func classTable() []class {
	compile := func(exprs ...string) []*regexp.Regexp {
		out := make([]*regexp.Regexp, len(exprs))
		for i, e := range exprs {
			out[i] = regexp.MustCompile(e)
		}

		return out
	}

	const anyLevel = 7

	return []class{
		{key: "kernel/machine-check", label: "machine check errors", maxLevel: anyLevel, re: compile(
			`\[Hardware Error\]`,
			`Machine check events logged`,
			`^mce: .*Machine Check`,
			`^mce: Uncorrected hardware memory error`,
			`^Memory failure: .*recovery action`,
		)},
		{key: "kernel/ata", label: "ATA errors", maxLevel: anyLevel, re: compile(
			`^ata\d+(\.\d+)?: SError: `,
			`^ata\d+(\.\d+)?: (hard |soft )?resetting link`,
			`^ata\d+: (COMRESET failed|link is slow to respond|limiting SATA link speed)`,
			`^ata\d+(\.\d+)?: (exception|failed command|hard resetting)`,
		)},
		{
			key:      "kernel/nvme",
			label:    "NVMe errors",
			maxLevel: anyLevel,
			re: compile(
				`^nvme( nvme)?\d+: I/O .*timeout`,
				`^nvme( nvme)?\d+: .*(resetting controller|controller is down|Device not ready|I/O error)`,
			),
		},
		{key: "kernel/thermal-throttle", label: "thermal throttling", maxLevel: anyLevel, re: compile(
			// "is above" since Linux 5.5; "mce: " when logged from the MCE code.
			`^(mce: )?(CPU\d+: )?(Core|Package) temperature (is )?above threshold`,
		)},
		{
			key:      "kernel/storage-io",
			label:    "storage I/O errors",
			maxLevel: anyLevel,
			re: compile(
				`I/O error`,
				`^(critical (medium|target|nexus)|timeout) error, dev `,
			),
			// exclude covers every storage-io pattern for loop, sr, fd and zram
			// devices, as the block layer (dev loop0) and filesystems ((device
			// loop0), XFS (loop0)) name them.
			exclude: regexp.MustCompile(`(\bdev |\bdevice |\()(loop|sr|fd|zram)\d`),
		},
	}
}

// Episode tracks one ongoing run of records of a class.
type Episode struct {
	FirstAt     time.Time `json:"first_at"`
	LastAt      time.Time `json:"last_at"`
	LastStillAt time.Time `json:"last_still_at"`
	Count       int       `json:"count"`
	// Unreported counts the occurrences since the last message of the episode.
	Unreported int `json:"unreported"`
	// Last is the latest record line, or the latest counter value of an EDAC episode.
	Last string `json:"last"`
}

// Cursor is the position in the kernel log that was fully processed.
type Cursor struct {
	BootID string `json:"boot_id"`
	Seq    uint64 `json:"seq"`
	Seen   bool   `json:"seen"` // false until a record was processed in this boot
	// Usec is the kmsg time since boot of the record at Seq, in microseconds; it matches the
	// journal's _SOURCE_MONOTONIC_TIMESTAMP.
	Usec uint64 `json:"usec"`
	// Message is the text of the record at Seq. It tells the journal records logged at Usec
	// that were processed from those that were not.
	Message string `json:"message,omitempty"`
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
	boot    BootSource
	edac    EDACSource
	journal JournalSource // nil: the previous boot's tail is skipped
	log     *zap.Logger
	marks   *health.RateMarks
	now     func() time.Time

	classes []class

	timeout time.Duration
	replay  bool

	mu      sync.Mutex
	st      state
	inbox   []entry // matching records consumed since the last Run
	hasSeen bool    // any record consumed in this process
	maxSeq  uint64  // highest Seq consumed, not yet committed to st.Cursor
	maxUsec uint64  // time since boot of the record at maxSeq, in microseconds
	maxMsg  string  // message of the record at maxSeq
	// firstSeq is the Seq of the first record consumed in this process; firstPending
	// is true until Run has compared it with the stored cursor.
	firstSeq     uint64
	firstPending bool
	lost         uint64 // records lost since the last report
	bootOK       bool
	bootID       string
	bootErr      error
}

// Option customises a Check.
type Option func(*Check)

// WithTimeout bounds every call into a source adapter (default 10 s).
func WithTimeout(d time.Duration) Option {
	return func(c *Check) { c.timeout = d }
}

// WithReplay makes the check judge the records it was handed as a whole, as a
// one-shot report: the stored cursor and episodes are ignored, nothing is
// committed to the state, and a fresh install is not silenced. Every class
// with matching records reads as found.
func WithReplay() Option {
	return func(c *Check) { c.replay = true }
}

// WithJournal makes the first Run after a reboot read the previous boot's kernel records
// after the cursor from the journal. Without it the previous boot's tail is skipped.
func WithJournal(j JournalSource) Option {
	return func(c *Check) { c.journal = j }
}

// WithLogger sets the logger (default: no logging). A nil logger keeps the default.
func WithLogger(log *zap.Logger) Option {
	return func(c *Check) {
		if log != nil {
			c.log = log
		}
	}
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
		log:     zap.NewNop(),
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
	if c.hasSeen {
		if r.Seq > c.maxSeq+1 {
			c.lost += r.Seq - c.maxSeq - 1
		}
	} else {
		c.firstSeq, c.firstPending = r.Seq, true
	}
	c.hasSeen = true
	c.maxSeq, c.maxUsec, c.maxMsg = r.Seq, usec(r.Time), r.Message
	if r.Facility != 0 {
		return
	}

	key, ok := classify(c.classes, r)
	switch {
	case !ok:
	case len(c.inbox) < maxInbox:
		c.inbox = append(c.inbox, entry{seq: r.Seq, key: key, msg: r.Message})
	default:
		c.lost++
	}
}

// classify returns the key of the first class that matches the record.
func classify(classes []class, r Record) (string, bool) {
	for _, cl := range classes {
		if r.Level > cl.maxLevel {
			continue
		}
		if cl.exclude != nil && cl.exclude.MatchString(r.Message) {
			continue
		}
		for _, re := range cl.re {
			if re.MatchString(r.Message) {
				return cl.key, true
			}
		}
	}

	return "", false
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

	var (
		readings []health.Reading
		tailErr  error
	)
	if !c.replay && c.st.Cursor.BootID != "" && c.st.Cursor.BootID != c.bootID {
		// The previous boot's tail is read once, before the cursor moves to this boot.
		readings, tailErr = c.bootTail(ctx)
		if err := ctx.Err(); err != nil {
			return health.CheckResult{Err: fmt.Errorf("kernel check: %w", err)}
		}
	}

	now := c.now()
	if c.replay {
		readings = c.replayReadings(c.takeInbox())
	} else {
		readings = append(readings, c.kernelReadings(now)...)
	}

	edacReadings, err := c.checkEDAC(ctx)
	readings = append(readings, edacReadings...)
	err = errors.Join(tailErr, err)
	if !c.replay {
		readings = append(readings, c.stillHappening(now)...)
	}

	if c.lost > 0 {
		lostErr := health.CheckError{
			Subject: "kmsg",
			Kind:    "lost",
			Err:     fmt.Errorf("%d kernel records were lost before they could be read", c.lost),
		}
		c.lost = 0
		err = errors.Join(lostErr, err)
	}

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
		c.st.Cursor = Cursor{BootID: c.bootID, Seq: c.maxSeq, Seen: true, Usec: c.maxUsec, Message: c.maxMsg}

		return nil
	}
	if c.st.Cursor.BootID != c.bootID {
		c.st.Cursor = Cursor{BootID: c.bootID}
	}

	cur := c.st.Cursor
	c.countLostWhileDown(cur)
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
	c.advanceCursor(cur)

	return c.drainRecords(now, agg)
}

// advanceCursor moves the stored cursor of this boot to the highest record consumed, unless
// it already points further. The caller holds c.mu.
func (c *Check) advanceCursor(cur Cursor) {
	if !c.hasSeen {
		return
	}
	if !cur.Seen || c.maxSeq > cur.Seq {
		c.st.Cursor.Seq, c.st.Cursor.Usec, c.st.Cursor.Message = c.maxSeq, c.maxUsec, c.maxMsg
	}
	c.st.Cursor.Seen = true
}

// countLostWhileDown compares the first record of this process with the stored
// cursor of the same boot: the records in between were overwritten while the
// daemon was down. The caller holds c.mu.
func (c *Check) countLostWhileDown(cur Cursor) {
	if !c.firstPending {
		return
	}
	c.firstPending = false
	if cur.Seen && c.firstSeq > cur.Seq+1 {
		c.lost += c.firstSeq - cur.Seq - 1
	}
}

// takeInbox hands out the buffered entries and empties the buffer.
func (c *Check) takeInbox() []entry {
	inbox := c.inbox
	c.inbox = nil

	return inbox
}

// aggregate counts the entries per class key, keeping the latest line of each.
func aggregate(entries []entry) map[string]*pending {
	agg := make(map[string]*pending)
	for _, e := range entries {
		p := agg[e.key]
		if p == nil {
			p = &pending{}
			agg[e.key] = p
		}
		p.count++
		p.sample = e.msg
	}

	return agg
}

// replayReadings reports every class the records match, once each, leaving the
// cursor and the episodes alone.
func (c *Check) replayReadings(inbox []entry) []health.Reading {
	agg := aggregate(inbox)

	var out []health.Reading
	for _, cl := range c.classes {
		if p := agg[cl.key]; p != nil {
			out = append(out, event(cl.key, fmt.Sprintf("Kernel: %s found in the kernel log (%s). Last: %s",
				cl.label, records(p.count), clip(p.sample))))
		}
	}

	return out
}

func (c *Check) drainRecords(now time.Time, agg map[string]*pending) []health.Reading {
	var out []health.Reading
	for _, cl := range c.classes {
		p := agg[cl.key]
		if p == nil {
			continue
		}

		out = append(out, c.episode(cl.key, now, p.count, clip(p.sample), func() string {
			return fmt.Sprintf("Kernel: %s started (%s). Last: %s", cl.label, records(p.count), clip(p.sample))
		})...)
	}

	return out
}

// episode applies the shared episode rules to n new occurrences under key: one
// message when an episode starts (none yet, or the last occurrence is at least
// quietAfter old). Later occurrences are only counted; stillHappening reports
// them. The caller holds c.mu.
func (c *Check) episode(key string, now time.Time, n int, last string, start func() string) []health.Reading {
	ep, ok := c.st.Episodes[key]
	if !ok || now.Sub(ep.LastAt) >= quietAfter {
		c.marks.Allow(key, channel, now, stillWindow)
		c.st.Episodes[key] = Episode{FirstAt: now, LastAt: now, LastStillAt: now, Count: n, Last: last}

		return []health.Reading{event(key, start())}
	}

	ep.LastAt = now
	ep.Count += n
	ep.Unreported += n
	ep.Last = last
	c.st.Episodes[key] = ep

	return nil
}

// stillHappening walks every episode, not only those that got a record in this
// Run: one with unreported occurrences and a last message at least stillWindow
// old gets a "still happening" message, and its unreported count is cleared.
// The caller holds c.mu.
func (c *Check) stillHappening(now time.Time) []health.Reading {
	keys := make([]string, 0, len(c.st.Episodes))
	for key := range c.st.Episodes {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	var out []health.Reading
	for _, key := range keys {
		ep := c.st.Episodes[key]
		if ep.Unreported <= 0 || now.Sub(ep.LastStillAt) < stillWindow {
			continue
		}
		if !c.marks.Allow(key, channel, now, stillWindow) {
			continue
		}
		out = append(out, event(key, c.stillText(key, ep)))
		ep.LastStillAt = now
		ep.Unreported = 0
		c.st.Episodes[key] = ep
	}

	return out
}

// stillText words the "still happening" message of the episode under key.
func (c *Check) stillText(key string, ep Episode) string {
	since := ep.LastStillAt.UTC().Format(time.RFC3339)
	if rest, ok := strings.CutPrefix(key, "kernel/edac/"); ok {
		i := strings.LastIndex(rest, "/")
		adj := map[string]string{"ce": "corrected", "ue": "uncorrected"}[rest[i+1:]]

		return fmt.Sprintf("Kernel: EDAC controller %s %s memory errors still rising (%d rises since %s, now %s)",
			rest[:i], adj, ep.Unreported, since, ep.Last)
	}

	label := key
	for _, cl := range c.classes {
		if cl.key == key {
			label = cl.label
		}
	}

	return fmt.Sprintf("Kernel: %s still happening (%s since %s). Last: %s",
		label, records(ep.Unreported), since, ep.Last)
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

	now := c.now()

	var (
		out  []health.Reading
		errs []error
	)
	for _, ctrl := range ctrls {
		if ctrl.Err != nil {
			// Keep the stored values, judge the other controllers and raise one
			// checker error under this controller's own key.
			errs = append(errs, health.CheckError{
				Subject: ctrl.Controller,
				Kind:    "edac-unreadable",
				Err:     fmt.Errorf("memory controller %s counters unreadable: %w", ctrl.Controller, ctrl.Err),
			})

			continue
		}
		prev, known := c.st.EDAC[ctrl.Controller]
		if known {
			id, cur := ctrl.Controller, ctrl
			out = append(out, c.edacRise(now, id, "ce", "corrected", prev.Correctable, cur.Correctable)...)
			if cur.Uncorrectable > prev.Uncorrectable {
				// Uncorrectable rises bypass the episodes: each one is reported at once.
				out = append(out, event("kernel/edac/"+id+"/ue", fmt.Sprintf(
					"Kernel: uncorrectable memory errors on %s rose %d→%d", id, prev.Uncorrectable, cur.Uncorrectable,
				)))
			}
		}
		c.st.EDAC[ctrl.Controller] = ctrl
	}

	return out, errors.Join(errs...)
}

// edacRise runs one correctable counter of one controller through the episode
// rules. A counter that did not rise (or was reset) changes nothing.
func (c *Check) edacRise(now time.Time, ctrl, kind, adj string, prev, cur uint64) []health.Reading {
	if cur <= prev {
		return nil
	}

	return c.episode("kernel/edac/"+ctrl+"/"+kind, now, 1, strconv.FormatUint(cur, 10), func() string {
		return fmt.Sprintf("Kernel: EDAC controller %s %s memory errors started rising (from %d to %d)",
			ctrl, adj, prev, cur)
	})
}

func event(key, text string) health.Reading {
	return health.Reading{Key: key, Verdict: health.Bad, Text: text, Kind: health.Event}
}

// usec converts a time since boot to whole microseconds.
func usec(d time.Duration) uint64 {
	return uint64(max(d, 0) / time.Microsecond)
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
