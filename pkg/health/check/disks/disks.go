package disks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/config"
	"github.com/Toshik1978/tgshd/pkg/health/store"
)

// Condition aspects of a drive, keyed disks/<serial>/<aspect>.
const (
	aspectHealth   = "health"
	aspectWarning  = "nvme-warning"
	aspectSpare    = "spare"
	aspectWear     = "wear"
	aspectSelfTest = "selftest"
	// aspectSelfTestFailure keys the event of a new failed self-test that is
	// not the newest finished one.
	aspectSelfTestFailure = "selftest-failure"
	aspectMissing         = "missing"
	aspectTemp            = "temp"
)

const (
	checkName = "disks"
	// kindNoVerdict is the checker-error kind of a read without a health verdict.
	kindNoVerdict = "no-verdict"
	// kindNoTemperature is the checker-error kind of a read without a
	// temperature.
	kindNoTemperature = "no-temperature"
	// inventoryKey is the event key of the first-run message listing all drives.
	inventoryKey = checkName + "/drives"

	// gapCapFactor bounds the time one over-temperature reading adds to its
	// streak, as a multiple of the check interval (research R1).
	gapCapFactor = 1.5

	day = 24 * time.Hour
)

// StandbyStatus is the exit status Source.Run reports for a drive in a
// low-power mode (sleep or standby) that was therefore not read:
// pkg/host/smartctl runs `smartctl -n standby,3`, smartctl(8)'s way to give
// that case a status of its own (its default, 2, also means an open failure).
const StandbyStatus = 3

// maxParallelReads bounds the drives read at the same time.
const maxParallelReads = 8

// probeTimeout bounds the reads of a run whose context has no deadline (each
// read is also bounded by the source's own per-drive timeout). Under the
// runner the run's deadline, less health.RunMargin, ends them first.
const probeTimeout = 5 * time.Minute

// asleepOrphanLimit bounds how long a node in standby that no drive can be
// attributed to (one never read awake since the daemon started) suspends the
// missing judgement: it may be a known drive that moved, but not for ever.
// The clock (Check.asleepSince) is kept in memory only, so a daemon restart
// restarts it and such a node suspends the judgement for another 24 h.
const asleepOrphanLimit = 24 * time.Hour

// errAsleep marks a drive in standby, not read this run; it is never reported.
var errAsleep = errors.New("drive in standby")

// ErrBaselinesNotLoaded is returned by Run until LoadBaselines has been
// called, so a check that was never restored cannot mistake itself for a first
// run and overwrite the stored baselines.
var ErrBaselinesNotLoaded = errors.New("drive baselines not loaded")

// Node is a drive's device node and its raw resolved sysfs path.
type Node struct {
	Name string
	Path string
}

// Source discovers drives and runs smartctl against one of them. It is
// implemented by pkg/host/smartctl.
type Source interface {
	// Drives returns the device nodes (names such as "sda", "nvme0n1") of the
	// physical drives.
	Drives() ([]Node, error)
	// Run returns smartctl's JSON output for the node name and its exit
	// status. wake reads a drive in standby (spinning it up) instead of
	// reporting StandbyStatus.
	Run(ctx context.Context, node string, wake bool) (stdout []byte, exitStatus int, err error)
}

// Check is the drive health check. It owns the per-drive baselines; the runner
// restores them with LoadBaselines and persists Baselines after each run.
type Check struct {
	src     Source
	cfg     config.Disks
	temps   *config.Temperatures
	now     func() time.Time
	exclude map[string]struct{}

	mu     sync.Mutex
	loaded bool // LoadBaselines was called; Run refuses to work before
	bases  map[string]*baseline
	hot    map[string]*streak
	// excludedNodes maps the node an excluded drive was last read from to
	// its serial, so that drive asleep there is credited to it (see
	// noteNodes and noteRead for when a credit is dropped).
	excludedNodes map[string]string
	// nodes is the set of nodes discovered in the last run, sorted and
	// joined.
	nodes string
	// asleepSince maps a node in standby that no drive could be attributed
	// to, in the last run, to when it was first found so. It lives in memory
	// only: a daemon restart restarts every node's clock.
	asleepSince map[string]time.Time
}

// baseline is the in-memory form of store.Baseline.
type baseline struct {
	serial    string
	model     string
	bus       string
	data      baselineData
	firstSeen time.Time
	lastSeen  time.Time
	retired   bool
}

// baselineData is the JSON kept in store.Baseline.Data.
type baselineData struct {
	// Counters are the error counters as last seen, keyed like Report.Counters.
	Counters map[string]int64 `json:"counters,omitempty"`
	// SelfTestOKAt is when a passed self-test was last newly observed (Unix
	// seconds); with the drive's first sighting it gives the calendar age of
	// the last pass.
	SelfTestOKAt int64 `json:"selftest_ok_at,omitempty"`
	// SelfTestHead is the identities of the newest log entries seen last
	// time, to recognise what is new on the next read.
	SelfTestHead []SelfTestID `json:"selftest_head,omitempty"`
	// SelfTestNewest is the newest finished self-test, for status while the
	// drive is not read.
	SelfTestNewest *SelfTestNewest `json:"selftest_newest,omitempty"`
	// SelfTestFormat is ok or unsupported (SelfTestFormat of the last read).
	SelfTestFormat SelfTestFormat `json:"selftest_format,omitempty"`
	// Health is the last read's SMART verdict: ok, failing or unknown.
	Health string `json:"health,omitempty"`
	// Node is the device node the drive was last read from; it attributes an
	// unreadable node to its drive.
	Node string `json:"node,omitempty"`
}

// streak tracks how long a drive has been above its temperature limit,
// counting observed time only.
type streak struct {
	badFor time.Duration
	last   time.Time
	start  time.Time // the run that first saw the drive over its limit
}

// probe is the outcome of reading one device node. A drive in standby was not
// read: its err is errAsleep.
type probe struct {
	node   string
	report Report
	err    error
}

// result accumulates the readings and samples of one run.
type result struct {
	res health.CheckResult
	// errs are the checker errors raised while judging, joined into res.Err.
	errs []error
}

// New returns a disks check. temps is the temperatures section: nil means
// drive temperatures are sampled but not judged. now is the clock.
func New(src Source, disks config.Disks, temps *config.Temperatures, now func() time.Time) *Check {
	exclude := make(map[string]struct{}, len(disks.ExcludeSerials))
	for _, s := range disks.ExcludeSerials {
		exclude[s] = struct{}{}
	}

	return &Check{
		src: src, cfg: disks, temps: temps, now: now, exclude: exclude,
		bases: map[string]*baseline{}, hot: map[string]*streak{},
		excludedNodes: map[string]string{}, asleepSince: map[string]time.Time{},
	}
}

// Run reads every discovered drive and judges it against its baseline. A
// failed discovery makes the whole run unreadable (Err, no readings). A drive
// that cannot be read gets no readings; its node (and serial, when known) is
// reported in Err, joined per node, alongside the healthy drives' readings. A
// drive in standby (StandbyStatus) is skipped: no readings, not missing, no
// error.
func (c *Check) Run(ctx context.Context) health.CheckResult {
	c.mu.Lock()
	loaded := c.loaded
	c.mu.Unlock()
	if !loaded {
		return health.CheckResult{Err: ErrBaselinesNotLoaded}
	}

	probes, err := c.probe(ctx)
	if err != nil {
		return health.CheckResult{Err: err}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return c.judge(c.now(), probes)
}

// LoadBaselines replaces the check's baselines with stored ones and must be
// called before the first Run; nil or empty means nothing is stored, so the
// next run is the first run. Data that cannot be decoded is dropped, so that
// drive is re-baselined silently.
func (c *Check) LoadBaselines(bases []store.Baseline) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.loaded = true
	c.bases = make(map[string]*baseline, len(bases))
	c.hot = map[string]*streak{}
	for i := range bases {
		sb := &bases[i]
		b := &baseline{
			serial: sb.Serial, model: sb.Model, bus: sb.Bus,
			firstSeen: sb.FirstSeenAt, lastSeen: sb.LastSeenAt, retired: sb.Retired,
		}
		if err := json.Unmarshal(sb.Data, &b.data); err != nil {
			b.data = baselineData{}
		}
		if b.data.Counters == nil {
			b.data.Counters = map[string]int64{}
		}
		c.bases[sb.Serial] = b
	}
}

// Baselines exports every baseline, retired ones included, sorted by serial.
func (c *Check) Baselines() []store.Baseline {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]store.Baseline, 0, len(c.bases))
	for _, serial := range slices.Sorted(maps.Keys(c.bases)) {
		b := c.bases[serial]
		data, err := json.Marshal(b.data)
		if err != nil {
			data = []byte("{}")
		}
		out = append(out, store.Baseline{
			Serial: b.serial, Model: b.model, Bus: b.bus, Data: data,
			FirstSeenAt: b.firstSeen, LastSeenAt: b.lastSeen, Retired: b.retired,
		})
	}

	return out
}

// Retire marks a known, not yet retired drive as retired: it is no longer
// judged missing, and announced as new if it reappears. It reports whether
// such a drive was found.
func (c *Check) Retire(serial string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	b, ok := c.bases[serial]
	if !ok || b.retired {
		return false
	}
	b.retired = true
	delete(c.hot, serial)

	return true
}

// probe discovers the drives and reads them concurrently (at most
// maxParallelReads at a time), so one slow drive, bounded by the source's own
// per-drive timeout, never holds up the others. A drive still unread when ctx
// ends is unreadable this run; the drives already read keep their results.
func (c *Check) probe(ctx context.Context) ([]probe, error) {
	nodes, err := c.src.Drives()
	if err != nil {
		return nil, fmt.Errorf("discover drives: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("read drives: %w", err)
	}
	// The reads end before the run's deadline (health.Bound), so a run that
	// started late in the tick reports its slow drives itself rather than
	// being abandoned with every drive's readings.
	ctx, cancel, _ := health.Bound(ctx, probeTimeout)
	defer cancel()

	out := make([]probe, len(nodes))
	sem := make(chan struct{}, maxParallelReads)
	var wg sync.WaitGroup
	for i, n := range nodes {
		node := n.Name
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				out[i] = probe{node: node, err: fmt.Errorf("%w: not read: %w", ErrUnreadable, ctx.Err())}

				return
			}
			out[i] = c.read(ctx, node)
		})
	}
	wg.Wait()

	return out, nil
}

func (c *Check) read(ctx context.Context, node string) probe {
	stdout, status, err := c.src.Run(ctx, node, false)
	if err != nil {
		return probe{node: node, err: fmt.Errorf("%w: run smartctl: %w", ErrUnreadable, err)}
	}
	if status == StandbyStatus {
		return probe{node: node, err: errAsleep}
	}
	// The serial is only known once the output is decoded, so decode with
	// every id any drive may track, then keep this drive's own set.
	report, err := DecodeSMART(stdout, status, c.allCounters())
	if err == nil {
		set := c.counterSet(report.Serial)
		for k := range report.Counters {
			if id, ok := ataCounterID(k); ok && !set[id] {
				delete(report.Counters, k)
			}
		}
	}

	return probe{node: node, report: report, err: err}
}

// counterSet returns the ATA attribute ids tracked for the drive with the
// serial: the built-in set, disks.extra_counters and the drive's own
// disks.extra_counters_by_serial.
func (c *Check) counterSet(serial string) map[int]bool {
	set := BuiltinCounters()
	for _, id := range c.cfg.ExtraCounters {
		set[id] = true
	}
	for _, id := range c.cfg.ExtraCountersBySerial[serial] {
		set[id] = true
	}

	return set
}

// allCounters is the union of the counter sets of every drive.
func (c *Check) allCounters() map[int]bool {
	set := c.counterSet("")
	for _, ids := range c.cfg.ExtraCountersBySerial {
		for _, id := range ids {
			set[id] = true
		}
	}

	return set
}

// ataCounterID is the inverse of ATACounter.
func ataCounterID(k string) (int, bool) {
	s, ok := strings.CutPrefix(k, "ata.")
	if !ok {
		return 0, false
	}
	id, err := strconv.Atoi(s)

	return id, err == nil
}

// judge turns one run's probes into readings and samples and updates the
// baselines. It must be called with c.mu held.
func (c *Check) judge(now time.Time, probes []probe) health.CheckResult {
	var r result
	first := len(c.bases) == 0
	seen := map[string]bool{}
	var inventory []string
	var unreadable []probe

	c.noteNodes(probes)
	for i := range probes {
		p := &probes[i]
		serial := p.report.Serial
		switch {
		case p.err != nil:
			unreadable = append(unreadable, *p)
		case c.excluded(serial) || seen[serial]:
		default:
			seen[serial] = true
			b, fresh := c.track(now, p)
			switch {
			case fresh && first:
				inventory = append(inventory, describe(p))
			case fresh:
				r.event(key(serial, "new"), "New drive: "+describe(p))
			}
			c.judgeDrive(&r, now, b, &p.report, fresh)
		}
	}

	errs, orphans := c.attribute(now, unreadable, seen)
	c.judgeMissing(&r, seen, orphans)
	r.res.Err = errors.Join(slices.Concat(r.errs, errs)...)

	if len(inventory) > 0 {
		text := fmt.Sprintf("Drives found (%d):\n%s", len(inventory), strings.Join(inventory, "\n"))
		ev := health.Reading{Key: inventoryKey, Verdict: health.Good, Text: text, Kind: health.Event}
		r.res.Readings = slices.Insert(r.res.Readings, 0, ev)
	}

	return r.res
}

func (c *Check) excluded(serial string) bool {
	_, ok := c.exclude[serial]
	return ok
}

// track returns the baseline of the drive just read, creating a fresh one for
// a drive never seen or retired; fresh reports that case.
func (c *Check) track(now time.Time, p *probe) (*baseline, bool) {
	serial := p.report.Serial
	b, ok := c.bases[serial]
	fresh := !ok || b.retired
	if fresh {
		b = &baseline{serial: serial, firstSeen: now, data: baselineData{Counters: map[string]int64{}}}
		c.bases[serial] = b
		delete(c.hot, serial)
	}
	b.model, b.bus = p.report.Model, string(p.report.Bus)
	b.lastSeen = now
	b.data.Node = p.node

	return b, fresh
}

func (c *Check) judgeDrive(r *result, now time.Time, b *baseline, rep *Report, fresh bool) {
	serial, name := rep.Serial, label(b)

	c.judgeVerdict(r, b, rep)
	c.judgeNVMe(r, name, rep)
	// A section the read lacked judges nothing: the conditions it feeds keep
	// their state, and counters are not seen as gone (FR-005).
	gaps := reportGaps(r, b, rep)
	if !gaps[GapSelfTestLog] {
		c.judgeSelfTest(r, now, b, rep)
	}
	c.judgeTemp(r, now, name, rep)
	if !gaps[GapAttributes] && !gaps[GapNVMeHealthLog] {
		judgeCounters(r, b, rep, fresh)
	}
	r.judged(serial, aspectMissing, false, "")

	if rep.NVMeErrorLogEntries != nil {
		r.sample("drive."+serial+".nvme_err_log", float64(*rep.NVMeErrorLogEntries))
	}
	if rep.DataWrittenBytes != nil {
		r.sample("drive."+serial+".written_bytes", float64(*rep.DataWrittenBytes))
	}
}

// reportGaps raises a checker error for every section the read lacked,
// keyed disks/<serial>/<section> (no-temperature for the temperature), and
// returns them as a set.
func reportGaps(r *result, b *baseline, rep *Report) map[Gap]bool {
	set := make(map[Gap]bool, len(rep.Gaps))
	for _, g := range rep.Gaps {
		set[g] = true
		kind := string(g)
		if g == GapTemperature {
			kind = kindNoTemperature
		}
		r.errs = append(r.errs, health.CheckError{
			Subject: rep.Serial, Kind: kind,
			Err: fmt.Errorf("%s: smartctl output has no %s", label(b), g),
		})
	}

	return set
}

// judgeVerdict judges the SMART overall health. A read without a verdict gets
// no reading at all, so an open problem stays open and nothing resolves, and a
// "no-verdict" checker error instead (FR-004).
func (c *Check) judgeVerdict(r *result, b *baseline, rep *Report) {
	b.data.Health = rep.Health.String()
	if rep.Health == HealthUnknown {
		r.errs = append(r.errs, health.CheckError{
			Subject: rep.Serial, Kind: kindNoVerdict,
			Err: fmt.Errorf("%s: smartctl reported no SMART health verdict", label(b)),
		})

		return
	}
	r.judged(rep.Serial, aspectHealth, rep.Health == HealthFailing, label(b)+": SMART overall health FAILED")
}

// judgeNVMe judges the NVMe health log fields the drive reports.
func (c *Check) judgeNVMe(r *result, name string, rep *Report) {
	serial := rep.Serial
	if w := rep.CriticalWarning; w != nil {
		r.judged(serial, aspectWarning, *w != 0, fmt.Sprintf("%s: NVMe critical warning 0x%02x", name, *w))
	}
	if spare := rep.AvailableSpare; spare != nil {
		c.judgeSpare(r, name, serial, *spare, rep.SpareThreshold)
		r.sample("drive."+serial+".spare", float64(*spare))
	}
	if used := rep.PercentageUsed; used != nil {
		r.judged(serial, aspectWear, *used > c.cfg.WearPercent,
			fmt.Sprintf("%s: wear %d%% above %d%%", name, *used, c.cfg.WearPercent))
		r.sample("drive."+serial+".wear", float64(*used))
	}
}

// judgeSpare judges the available spare against the drive's own threshold and
// the configured floor, whichever is reached first. The text names the drive's
// threshold when the spare is at or below it, otherwise the floor; a drive
// without a threshold is judged against the floor alone (FR-008).
func (c *Check) judgeSpare(r *result, name, serial string, spare int, th *int) {
	floor := c.cfg.SpareFloorPercent
	switch {
	case th != nil && spare <= *th:
		r.state(serial, aspectSpare, health.Bad,
			fmt.Sprintf("%s: available spare %d%% at or below its threshold %d%%", name, spare, *th))
	case spare < floor:
		r.state(serial, aspectSpare, health.Bad,
			fmt.Sprintf("%s: available spare %d%% below the %d%% floor", name, spare, floor))
	default:
		r.state(serial, aspectSpare, health.Good, "")
	}
}

// judgeSelfTest judges the self-test log against the baseline. A log that
// cannot be decoded (unsupported) is not judged: no reading, no error. Otherwise
// the condition is Bad when the newest finished self-test failed or the last
// observed pass is older than selftest_max_age by the calendar (a drive first
// seen less than that ago has not yet had the chance to pass). Each new failed
// entry that is not the newest finished one is reported once as an event, and
// the calendar age is sampled as drive.<serial>.selftest_age_h.
func (c *Check) judgeSelfTest(r *result, now time.Time, b *baseline, rep *Report) {
	serial, name := rep.Serial, label(b)
	b.data.SelfTestFormat = rep.SelfTestFormat
	if rep.SelfTestFormat == SelfTestFormatUnsupported {
		return
	}

	upd := AlignSelfTests(SelfTestState{Head: b.data.SelfTestHead, OKAt: b.data.SelfTestOKAt}, rep.SelfTestLog, now)
	b.data.SelfTestHead, b.data.SelfTestOKAt, b.data.SelfTestNewest = upd.Head, upd.OKAt, upd.Newest
	for _, f := range upd.Failures {
		r.event(key(serial, aspectSelfTestFailure), fmt.Sprintf(
			"%s: self-test failed (%s) at %d h; a newer test has passed since", name, f.Description, f.ID.Hours,
		))
	}

	age := SelfTestAge(upd.OKAt, b.firstSeen, now)
	r.sample("drive."+serial+".selftest_age_h", age.Hours())
	if n := upd.Newest; n != nil && n.Result == "failed" {
		text := fmt.Sprintf("%s: newest self-test failed (%s)", name, n.Description)
		r.state(serial, aspectSelfTest, health.Bad, text)
		return
	}
	maxAge := c.cfg.SelftestMaxAge.Duration()
	r.judged(serial, aspectSelfTest, age > maxAge, fmt.Sprintf(
		"%s: last passed self-test %d d ago, limit %d d", name, int64(age/day), int64(maxAge/day),
	))
}

// judgeTemp samples the drive temperature and judges it against the limit of
// its bus. The temperatures threshold is applied here, counting only observed
// time like the engine does: until the drive has been too hot for that long
// the condition is not judged at all (no reading).
func (c *Check) judgeTemp(r *result, now time.Time, name string, rep *Report) {
	t := rep.Temperature
	if t == nil {
		return
	}
	serial := rep.Serial
	r.sample("temp.drive."+serial, float64(*t))
	if c.temps == nil {
		return
	}

	limit := c.temps.Limits.DriveSATA
	if rep.Bus == BusNVMe {
		limit = c.temps.Limits.DriveNVMe
	}
	if *t <= limit {
		delete(c.hot, serial)
		r.judged(serial, aspectTemp, false, "")

		return
	}

	if over, since := c.overFor(now, serial); over >= c.temps.Threshold.Duration() {
		r.state(serial, aspectTemp, health.Bad, fmt.Sprintf("%s: temperature %d°C above %d°C", name, *t, limit))
		r.res.Readings[len(r.res.Readings)-1].Since = since
	}
}

// overFor extends the drive's over-temperature streak to now and returns its
// length and start; the time one reading adds is capped like the engine's gap
// cap.
func (c *Check) overFor(now time.Time, serial string) (time.Duration, time.Time) {
	s, ok := c.hot[serial]
	if !ok {
		c.hot[serial] = &streak{last: now, start: now}
		return 0, now
	}
	gapCap := time.Duration(gapCapFactor * float64(c.cfg.Interval.Duration()))
	s.badFor += max(min(now.Sub(s.last), gapCap), 0)
	s.last = now

	return s.badFor, s.start
}

// attribute gives each unreadable node to the known drive last read from it,
// so that drive is neither judged nor reported missing in this run. It returns
// one error per unreadable node, naming the node and, when attributed, the
// drive, plus how many nodes suspend the missing judgement.
//
// A node in standby (errAsleep) is attributed the same way but is not an
// error: not reading a sleeping drive is the point, and its wear and
// self-test simply wait for a run that finds it awake. A node no known drive
// can be attributed to is handled by orphan.
func (c *Check) attribute(now time.Time, unreadable []probe, seen map[string]bool) ([]error, int) {
	errs := make([]error, 0, len(unreadable))
	orphans := 0
	asleepSince := make(map[string]time.Time, len(c.asleepSince))
	for i := range unreadable {
		p := &unreadable[i]
		b := c.byNode(p.node, seen)
		if b == nil {
			suspends, err := c.orphan(now, p, asleepSince)
			if err != nil {
				errs = append(errs, err)
			}
			if suspends {
				orphans++
			}

			continue
		}
		seen[b.serial] = true
		if !errors.Is(p.err, errAsleep) {
			errs = append(errs, fmt.Errorf("%s on /dev/%s: %w", label(b), p.node, p.err))
		}
	}
	c.asleepSince = asleepSince

	return errs, orphans
}

// orphan judges an unreadable node no known drive can be attributed to. It
// returns whether it suspends the missing judgement this run, and its checker
// error (nil when none):
//   - a node an excluded drive was last read from (excludedNodes) does not
//     suspend it; asleep it is credited to that drive silently, unreadable it
//     is a checker error naming that drive;
//   - any other node in standby suspends it, silently, only for its first
//     asleepOrphanLimit asleep (asleepSince, kept in next);
//   - any other unreadable node suspends it, with a checker error.
func (c *Check) orphan(now time.Time, p *probe, next map[string]time.Time) (bool, error) {
	asleep := errors.Is(p.err, errAsleep)
	ex, excluded := c.excludedNodes[p.node]
	switch {
	case excluded && asleep:
		return false, nil
	case excluded:
		return false, fmt.Errorf("/dev/%s (last read as excluded drive %s): %w", p.node, ex, p.err)
	case asleep:
		return c.asleepOrphan(now, p.node, next), nil
	}

	return true, fmt.Errorf("/dev/%s (missing-drive check suspended this run): %w", p.node, p.err)
}

// noteNodes updates the excluded-drive credits for this run's probes. Every
// credit is dropped when the set of discovered nodes changed since the last
// run: drives were pulled, added or re-enumerated, so a node an excluded
// drive was read from may now be another drive's. Then each drive read
// awake updates them (noteRead).
func (c *Check) noteNodes(probes []probe) {
	names := make([]string, len(probes))
	for i := range probes {
		names[i] = probes[i].node
	}
	slices.Sort(names)
	nodes := strings.Join(names, " ")
	if nodes != c.nodes {
		clear(c.excludedNodes)
	}
	c.nodes = nodes
	for i := range probes {
		if probes[i].err == nil {
			c.noteRead(&probes[i])
		}
	}
}

// noteRead updates the excluded-drive credits for a drive read on p.node:
// an excluded drive moves its credit there, any other drive takes the
// node's credit away.
func (c *Check) noteRead(p *probe) {
	serial := p.report.Serial
	if !c.excluded(serial) {
		delete(c.excludedNodes, p.node)

		return
	}
	maps.DeleteFunc(c.excludedNodes, func(_, s string) bool { return s == serial })
	c.excludedNodes[p.node] = serial
}

// asleepOrphan records node, in standby and attributed to no drive, in next
// with the time it was first found so, and reports whether it still suspends
// the missing judgement (for less than asleepOrphanLimit).
func (c *Check) asleepOrphan(now time.Time, node string, next map[string]time.Time) bool {
	since, ok := c.asleepSince[node]
	if !ok {
		since = now
	}
	next[node] = since

	return now.Sub(since) < asleepOrphanLimit
}

// byNode returns the active, not yet seen drive last read from node.
func (c *Check) byNode(node string, seen map[string]bool) *baseline {
	for _, serial := range slices.Sorted(maps.Keys(c.bases)) {
		b := c.bases[serial]
		if b.data.Node == node && !b.retired && !seen[serial] && !c.excluded(serial) {
			return b
		}
	}

	return nil
}

// judgeMissing raises a problem for every known, active drive not seen in this
// run. While some node could not be read and attributed (orphans), an unseen
// drive may be that node, so missing is not judged at all (no reading) in
// that run.
func (c *Check) judgeMissing(r *result, seen map[string]bool, orphans int) {
	if orphans > 0 {
		return
	}
	for _, serial := range slices.Sorted(maps.Keys(c.bases)) {
		b := c.bases[serial]
		if b.retired || seen[serial] || c.excluded(serial) {
			continue
		}
		r.state(serial, aspectMissing, health.Bad, label(b)+" is missing")
	}
}

// judgeCounters emits one event per error counter that rose against the
// baseline and moves the baseline to the current values. A fresh drive and a
// counter reported for the first time only set the baseline.
func judgeCounters(r *result, b *baseline, rep *Report, fresh bool) {
	// The NVMe error log count used to be a counter; it is history only now.
	delete(b.data.Counters, legacyNVMeErrorLog)
	for _, k := range slices.Sorted(maps.Keys(rep.Counters)) {
		v := clampInt64(rep.Counters[k])
		old, known := b.data.Counters[k]
		b.data.Counters[k] = v
		if fresh || !known || v <= old {
			continue
		}
		r.event(key(rep.Serial, k), fmt.Sprintf("%s: %s rose %d→%d", label(b), counterLabel(k), old, v))
	}
}

func (r *result) state(serial, aspect string, v health.Verdict, text string) {
	r.res.Readings = append(r.res.Readings, health.Reading{
		Key: key(serial, aspect), Verdict: v, Text: text, Kind: health.State,
	})
}

// judged adds a state reading that is Bad with text when bad, else Good.
func (r *result) judged(serial, aspect string, bad bool, text string) {
	if bad {
		r.state(serial, aspect, health.Bad, text)
		return
	}
	r.state(serial, aspect, health.Good, "")
}

func (r *result) event(k, text string) {
	r.res.Readings = append(r.res.Readings, health.Reading{
		Key: k, Verdict: health.Good, Text: text, Kind: health.Event,
	})
}

func (r *result) sample(metric string, v float64) {
	r.res.Samples = append(r.res.Samples, health.Sample{Metric: metric, Value: v})
}

func key(serial, aspect string) string {
	return checkName + "/" + serial + "/" + aspect
}

func label(b *baseline) string {
	return fmt.Sprintf("drive %s (%s)", b.serial, b.model)
}

func describe(p *probe) string {
	return fmt.Sprintf("/dev/%s: %s %s, serial %s", p.node, p.report.Bus, p.report.Model, p.report.Serial)
}

// legacyNVMeErrorLog is the baseline key of the NVMe error log count, which
// was an error counter before it became a history-only sample.
const legacyNVMeErrorLog = "nvme.num_err_log_entries"

// counterLabel is counterName with the ATA attribute id, as shown in events.
func counterLabel(k string) string {
	if id, ok := ataCounterID(k); ok {
		return fmt.Sprintf("%s (ATA %d)", counterName(k), id)
	}

	return counterName(k)
}

// counterName is the human name of a Report.Counters key.
func counterName(k string) string {
	if k == CounterNVMeMediaErrors {
		return "media errors (NVMe)"
	}
	id, ok := ataCounterID(k)
	if !ok {
		return k
	}
	if n := knownATAName(id); n != "" {
		return n
	}

	return "attribute " + strconv.Itoa(id)
}

func knownATAName(id int) string {
	return map[int]string{
		5:   "reallocated sectors",
		179: "used reserved blocks",
		181: "program fail",
		182: "erase fail",
		183: "runtime bad blocks",
		187: "reported uncorrectable errors",
		188: "command timeouts",
		195: "hardware ECC",
		197: "pending sectors",
		198: "offline uncorrectable sectors",
		199: "UDMA CRC errors",
	}[id]
}

func clampInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}

	return int64(v)
}
