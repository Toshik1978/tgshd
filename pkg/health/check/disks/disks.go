package disks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
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
	aspectMissing  = "missing"
	aspectTemp     = "temp"
)

const (
	checkName = "disks"
	// inventoryKey is the event key of the first-run message listing all drives.
	inventoryKey = checkName + "/drives"

	// gapCapFactor bounds the time one over-temperature reading adds to its
	// streak, as a multiple of the check interval (research R1).
	gapCapFactor = 1.5

	// ataLifetimeWrap is where the 16-bit lifetime hours of the ATA self-test
	// log wrap around.
	ataLifetimeWrap = 1 << 16
)

// ErrBaselinesNotLoaded is returned by Run until LoadBaselines has been
// called, so a check that was never restored cannot mistake itself for a first
// run and overwrite the stored baselines.
var ErrBaselinesNotLoaded = errors.New("drive baselines not loaded")

// Source discovers drives and runs smartctl against one of them. It is
// implemented by pkg/host/smartctl.
type Source interface {
	// Drives returns the device node names (for example "sda", "nvme0n1") of
	// the physical drives.
	Drives() ([]string, error)
	// Run returns smartctl's JSON output for node and its exit status.
	Run(ctx context.Context, node string) (stdout []byte, exitStatus int, err error)
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
	// SelfTestLastSeenHours is the power-on hours of the newest finished
	// self-test judged. Informational: kept for the data model and reports,
	// not used in judgement (the age comes from the current SMART data).
	SelfTestLastSeenHours int64 `json:"selftest_last_seen_hours,omitempty"`
	// SelfTestOKAt is when a passed self-test was first seen (Unix seconds).
	// Informational, like SelfTestLastSeenHours.
	SelfTestOKAt int64 `json:"selftest_ok_at,omitempty"`
	// Node is the device node the drive was last read from; it attributes an
	// unreadable node to its drive.
	Node string `json:"node,omitempty"`
}

// streak tracks how long a drive has been above its temperature limit,
// counting observed time only.
type streak struct {
	badFor time.Duration
	last   time.Time
}

// probe is the outcome of reading one device node.
type probe struct {
	node   string
	report Report
	err    error
}

// result accumulates the readings and samples of one run.
type result struct {
	res health.CheckResult
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
	}
}

// Run reads every discovered drive and judges it against its baseline. A
// failed discovery makes the whole run unreadable (Err, no readings). A drive
// that cannot be read gets no readings; its node (and serial, when known) is
// reported in Err, joined per node, alongside the healthy drives' readings.
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

// probe discovers the drives and reads each of them.
func (c *Check) probe(ctx context.Context) ([]probe, error) {
	nodes, err := c.src.Drives()
	if err != nil {
		return nil, fmt.Errorf("discover drives: %w", err)
	}
	out := make([]probe, 0, len(nodes))
	for _, node := range nodes {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("read drives: %w", err)
		}
		out = append(out, c.read(ctx, node))
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("read drives: %w", err)
	}

	return out, nil
}

func (c *Check) read(ctx context.Context, node string) probe {
	stdout, status, err := c.src.Run(ctx, node)
	if err != nil {
		return probe{node: node, err: fmt.Errorf("%w: run smartctl: %w", ErrUnreadable, err)}
	}
	report, err := DecodeSMART(stdout, status)

	return probe{node: node, report: report, err: err}
}

// judge turns one run's probes into readings and samples and updates the
// baselines. It must be called with c.mu held.
func (c *Check) judge(now time.Time, probes []probe) health.CheckResult {
	var r result
	first := len(c.bases) == 0
	seen := map[string]bool{}
	var inventory []string
	var unreadable []probe

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

	errs, orphans := c.attribute(unreadable, seen)
	c.judgeMissing(&r, seen, orphans)
	r.res.Err = errors.Join(errs...)

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

	r.judged(serial, aspectHealth, rep.HealthFailed, name+": SMART overall health FAILED")
	c.judgeNVMe(r, name, rep)
	c.judgeSelfTest(r, now, b, rep)
	c.judgeTemp(r, now, name, rep)
	judgeCounters(r, b, rep, fresh)
	r.judged(serial, aspectMissing, false, "")

	if rep.DataWrittenBytes != nil {
		r.sample("drive."+serial+".written_bytes", float64(*rep.DataWrittenBytes))
	}
}

// judgeNVMe judges the NVMe health log fields the drive reports.
func (c *Check) judgeNVMe(r *result, name string, rep *Report) {
	serial := rep.Serial
	if w := rep.CriticalWarning; w != nil {
		r.judged(serial, aspectWarning, *w != 0, fmt.Sprintf("%s: NVMe critical warning 0x%02x", name, *w))
	}
	if spare, th := rep.AvailableSpare, rep.SpareThreshold; spare != nil && th != nil {
		r.judged(serial, aspectSpare, *spare <= *th,
			fmt.Sprintf("%s: available spare %d%% at or below its threshold %d%%", name, *spare, *th))
	}
	if spare := rep.AvailableSpare; spare != nil {
		r.sample("drive."+serial+".spare", float64(*spare))
	}
	if used := rep.PercentageUsed; used != nil {
		r.judged(serial, aspectWear, *used > c.cfg.WearPercent,
			fmt.Sprintf("%s: wear %d%% above %d%%", name, *used, c.cfg.WearPercent))
		r.sample("drive."+serial+".wear", float64(*used))
	}
}

// judgeSelfTest raises a problem when the newest finished self-test failed or
// is older than selftest_max_age in power-on hours, or when there is none and
// the drive was first seen longer than selftest_max_age ago. Otherwise, when
// there is no self-test yet or its age cannot be computed, it is not judged.
// The newest finished self-test's age in power-on hours is sampled as
// drive.<serial>.selftest_age_h whenever it can be computed.
func (c *Check) judgeSelfTest(r *result, now time.Time, b *baseline, rep *Report) {
	serial, name := rep.Serial, label(b)
	maxAge := c.cfg.SelftestMaxAge.Duration()
	st := rep.SelfTest
	if st == nil {
		if known := now.Sub(b.firstSeen); known > maxAge {
			r.state(serial, aspectSelfTest, health.Bad, fmt.Sprintf(
				"%s: no finished self-test in the log, first seen %d h ago", name, int64(known/time.Hour),
			))
		}

		return
	}
	b.noteSelfTest(now, st)
	age, ok := selfTestAge(rep)
	if ok {
		r.sample("drive."+serial+".selftest_age_h", float64(age))
	}
	if st.Failed {
		r.judged(serial, aspectSelfTest, true, fmt.Sprintf("%s: newest self-test failed (%s)", name, st.Description))
		return
	}
	if !ok {
		return
	}
	limit := int64(maxAge / time.Hour)
	r.judged(serial, aspectSelfTest, age > limit,
		fmt.Sprintf("%s: newest finished self-test ran %d h ago, limit %d h", name, age, limit))
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

	if c.overFor(now, serial) >= c.temps.Threshold.Duration() {
		r.state(serial, aspectTemp, health.Bad, fmt.Sprintf("%s: temperature %d°C above %d°C", name, *t, limit))
	}
}

// overFor extends the drive's over-temperature streak to now and returns its
// length; the time one reading adds is capped like the engine's gap cap.
func (c *Check) overFor(now time.Time, serial string) time.Duration {
	s, ok := c.hot[serial]
	if !ok {
		c.hot[serial] = &streak{last: now}
		return 0
	}
	gapCap := time.Duration(gapCapFactor * float64(c.cfg.Interval.Duration()))
	s.badFor += max(min(now.Sub(s.last), gapCap), 0)
	s.last = now

	return s.badFor
}

// attribute gives each unreadable node to the known drive last read from it,
// so that drive is neither judged nor reported missing in this run. It returns
// one error per unreadable node, naming the node and, when attributed, the
// drive, plus how many nodes could not be attributed.
func (c *Check) attribute(unreadable []probe, seen map[string]bool) ([]error, int) {
	errs := make([]error, 0, len(unreadable))
	orphans := 0
	for i := range unreadable {
		p := &unreadable[i]
		b := c.byNode(p.node, seen)
		if b == nil {
			orphans++
			errs = append(errs, fmt.Errorf("/dev/%s (missing-drive check suspended this run): %w", p.node, p.err))

			continue
		}
		seen[b.serial] = true
		errs = append(errs, fmt.Errorf("%s on /dev/%s: %w", label(b), p.node, p.err))
	}

	return errs, orphans
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
// run. While some node could not be read and attributed, an unseen drive may
// be that node, so missing is not judged at all (no reading) in that run.
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
	for _, k := range slices.Sorted(maps.Keys(rep.Counters)) {
		v := clampInt64(rep.Counters[k])
		old, known := b.data.Counters[k]
		b.data.Counters[k] = v
		if fresh || !known || v <= old {
			continue
		}
		r.event(key(rep.Serial, k), fmt.Sprintf("%s: %s rose %d→%d", label(b), counterName(k), old, v))
	}
}

// noteSelfTest records the newest finished self-test in the baseline.
func (b *baseline) noteSelfTest(now time.Time, st *SelfTest) {
	if st.PowerOnHours == nil {
		return
	}
	hours := *st.PowerOnHours
	if !st.Failed && (hours != b.data.SelfTestLastSeenHours || b.data.SelfTestOKAt == 0) {
		b.data.SelfTestOKAt = now.Unix()
	}
	b.data.SelfTestLastSeenHours = hours
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

// selfTestAge returns the power-on hours elapsed since the newest finished
// self-test. The ATA log keeps 16-bit lifetime hours, so a drive past 65535
// hours is compared modulo 65536.
func selfTestAge(rep *Report) (int64, bool) {
	if rep.PowerOnHours == nil || rep.SelfTest.PowerOnHours == nil {
		return 0, false
	}
	now, at := *rep.PowerOnHours, *rep.SelfTest.PowerOnHours
	if rep.Bus == BusATA && now >= ataLifetimeWrap && at < ataLifetimeWrap {
		return ((now-at)%ataLifetimeWrap + ataLifetimeWrap) % ataLifetimeWrap, true
	}

	return max(now-at, 0), true
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

// counterName is the human name of a Report.Counters key.
func counterName(k string) string {
	switch k {
	case ATACounter(5):
		return "reallocated sectors (ATA 5)"
	case ATACounter(187):
		return "reported uncorrectable errors (ATA 187)"
	case ATACounter(188):
		return "command timeouts (ATA 188)"
	case ATACounter(197):
		return "pending sectors (ATA 197)"
	case ATACounter(198):
		return "offline uncorrectable sectors (ATA 198)"
	case ATACounter(199):
		return "UDMA CRC errors (ATA 199)"
	case CounterNVMeMediaErrors:
		return "media errors (NVMe)"
	case CounterNVMeErrorLog:
		return "error log entries (NVMe)"
	}

	return k
}

func clampInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}

	return int64(v)
}
