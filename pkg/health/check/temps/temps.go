// Package temps judges CPU, board and GPU temperatures. Drive temperatures are
// judged by the disks check from SMART data.
package temps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/config"
)

const gpuTimeout = 30 * time.Second

// Sensor classes, the second segment of a sample metric.
const (
	classCPU   = "cpu"
	classBoard = "board"
)

// The plausible physical range of a temperature reading. A value outside it is
// a reading fault, not heat.
const (
	minPlausible = -40.0
	maxPlausible = 150.0
)

// unsetCrit is a common "no limit" marker in temp<n>_crit; like a value
// outside (0, maxPlausible] it means the sensor exposes no critical limit.
const unsetCrit = 127.0

// Why a sensor that was judged before is no longer judged; the tail of the
// one event sent when it leaves an open problem behind.
const (
	retiredCPU   = " is no longer judged separately; the package sensor covers it"
	retiredBoard = " is no longer judged: it is not listed and exposes no critical limit"
)

// errNoCPUSensor is the cause of the temps/cpu/none checker error.
var errNoCPUSensor = errors.New("no CPU package sensor in the hwmon read")

// Sensor is one hwmon temperature input. Device is the resolved hwmon device
// path. Err is set when the input could not be read; Celsius is then meaningless.
type Sensor struct {
	Chip    string
	Label   string
	Device  string
	Celsius float64
	Max     *float64
	Crit    *float64
	Err     error
}

// ErrNoGPUs is returned by Nvidia.GPUs when the driver reports no GPU at all.
var ErrNoGPUs = errors.New("no GPUs")

// GPU is one NVIDIA GPU temperature. Celsius is nil when the GPU reports no reading.
type GPU struct {
	Index   int
	Name    string
	Celsius *float64
}

// Hwmon reads the hwmon sensors.
type Hwmon interface {
	Sensors() ([]Sensor, error)
}

// Nvidia reads the NVIDIA GPU temperatures.
type Nvidia interface {
	GPUs(ctx context.Context) ([]GPU, error)
}

// OpenState answers whether a condition has an open problem, so that a sensor
// that is gone is reported only when it leaves a problem behind.
type OpenState interface {
	IsOpen(key string) bool
}

// neverOpen is the OpenState of a check built without one.
type neverOpen struct{}

func (neverOpen) IsOpen(string) bool { return false }

// Option configures a Check.
type Option func(*Check)

// WithClock sets the clock that dates unreadable sensors (default time.Now).
func WithClock(now func() time.Time) Option {
	return func(c *Check) { c.now = now }
}

// UnreadableSensor is a judged sensor that has given no usable reading since
// Since: an input that cannot be read, or a value outside the plausible range.
type UnreadableSensor struct {
	Key, Name string
	Since     time.Time
}

// SensorStatus is what /temp shows of a sensor besides its stored samples.
type SensorStatus struct {
	// Metric is the sample metric ("temp.<class>.<chip>.<label>"); the GPU
	// metric is "temp.gpu.<index>".
	Metric string
	// Label is the sensor's name in /temp: chip and label as in the metric.
	Label string
	// Limit is the temperature the sensor is judged against; 0 means none
	// (an unlisted board sensor without a critical limit is only sampled).
	Limit float64
	// Since is when the sensor stopped giving a usable reading; zero when it does.
	Since time.Time
}

type source int

const (
	sourceHwmon source = iota
	sourceGPU
)

// Check is the temperatures check.
type Check struct {
	hwmon  Hwmon
	nvidia Nvidia
	limits config.Limits
	board  []config.BoardSensor
	open   OpenState
	now    func() time.Time

	mu   sync.Mutex
	seen [2]map[string]string // per source: condition key -> display name
	// devices maps an hwmon condition key to the device path of its chip,
	// to tell a gone chip from a sensor input that vanished.
	devices map[string]string
	// since maps the key of a sensor that gives no reading to when it stopped.
	since map[string]time.Time
	// info is what the last successful read of each source showed of its
	// sensors, by condition key. It is not persisted.
	info [2]map[string]SensorStatus
}

// New returns a temperatures check. Either source may be nil. open answers
// whether a sensor's condition is open; nil means never.
func New(hwmon Hwmon, nvidia Nvidia, cfg config.Temperatures, open OpenState, opts ...Option) *Check {
	if open == nil {
		open = neverOpen{}
	}
	c := &Check{
		hwmon:   hwmon,
		nvidia:  nvidia,
		limits:  cfg.Limits,
		board:   cfg.BoardSensors,
		open:    open,
		now:     time.Now,
		seen:    [2]map[string]string{{}, {}},
		devices: map[string]string{},
		since:   map[string]time.Time{},
		info:    [2]map[string]SensorStatus{{}, {}},
	}
	for _, o := range opts {
		o(c)
	}

	return c
}

// observed is one sensor reading. A limit of 0 means no limit. A sampleOnly
// sensor is sampled but never judged.
type observed struct {
	key, name, metric string
	celsius, limit    float64
	sampleOnly        bool
	// device is the device path of an hwmon sensor's chip.
	device string
	// unreadable says why the sensor gave no reading; empty when it did.
	unreadable string
}

// Run reads every source and judges each sensor against its limit. The
// reads run without the lock, so a hung sysfs or driver read never blocks
// Export (the checkpoint) or /temp.
func (c *Check) Run(ctx context.Context) health.CheckResult {
	now := c.now()
	var (
		sensors  []Sensor
		hwmonErr error
		gpus     []GPU
		gpuErr   error
	)
	if c.hwmon != nil {
		sensors, hwmonErr = c.hwmon.Sensors()
	}
	if c.nvidia != nil {
		gctx, cancel := context.WithTimeout(ctx, gpuTimeout)
		gpus, gpuErr = c.nvidia.GPUs(gctx)
		cancel()
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	var (
		res  health.CheckResult
		errs []error
	)
	if c.hwmon != nil {
		errs = c.applyHwmon(&res, now, sensors, hwmonErr, errs)
	}
	if c.nvidia != nil {
		errs = c.applyGPUs(&res, now, gpus, gpuErr, errs)
	}
	res.Err = errors.Join(errs...)

	return res
}

// Sensors returns the sensors the last reads showed, sorted by metric, with
// their limits and, for those without a usable reading, since when. It is empty
// until the first successful read.
func (c *Check) Sensors() []SensorStatus {
	c.mu.Lock()
	defer c.mu.Unlock()

	var out []SensorStatus
	for _, infos := range c.info {
		for key, st := range infos {
			st.Since = c.since[key]
			out = append(out, st)
		}
	}
	slices.SortFunc(out, func(a, b SensorStatus) int { return strings.Compare(a.Metric, b.Metric) })

	return out
}

// Unreadable returns the judged sensors that give no usable reading, sorted by
// key: an input that cannot be read, or a value outside the plausible range
// (both set the since).
func (c *Check) Unreadable() []UnreadableSensor {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]UnreadableSensor, 0, len(c.since))
	for _, key := range slices.Sorted(maps.Keys(c.since)) {
		name, ok := c.seen[sourceHwmon][key]
		if !ok {
			name = c.seen[sourceGPU][key]
		}
		out = append(out, UnreadableSensor{Key: key, Name: name, Since: c.since[key]})
	}

	return out
}

type seenState struct {
	Hwmon      map[string]string    `json:"hwmon"`
	GPU        map[string]string    `json:"gpu"`
	Devices    map[string]string    `json:"devices,omitempty"`
	Unreadable map[string]time.Time `json:"unreadable,omitempty"`
}

// Export serialises the set of known sensors, their chips' device paths and
// since when each unreadable one gives no reading, to be kept in the store so
// that gone detection survives a restart.
func (c *Check) Export() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := json.Marshal(seenState{
		Hwmon: c.seen[sourceHwmon], GPU: c.seen[sourceGPU], Devices: c.devices, Unreadable: c.since,
	})
	if err != nil {
		return "", fmt.Errorf("marshal seen sensors: %w", err)
	}

	return string(b), nil
}

// Restore loads a set produced by Export. An empty string is a no-op.
func (c *Check) Restore(data string) error {
	if data == "" {
		return nil
	}
	var st seenState
	if err := json.Unmarshal([]byte(data), &st); err != nil {
		return fmt.Errorf("unmarshal seen sensors: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen[sourceHwmon] = nonNil(st.Hwmon)
	c.seen[sourceGPU] = nonNil(st.GPU)
	c.devices = nonNil(st.Devices)
	c.since = st.Unreadable
	if c.since == nil {
		c.since = map[string]time.Time{}
	}

	return nil
}

// applyHwmon judges one hwmon read and returns errs with its errors added.
// The caller holds the lock.
func (c *Check) applyHwmon(
	res *health.CheckResult, now time.Time, sensors []Sensor, err error, errs []error,
) []error {
	if err != nil {
		errs = append(errs, fmt.Errorf("read hwmon: %w", err))
	}
	rd := c.hwmonObserved(sensors)
	if err == nil && !rd.hasCPU {
		errs = append(errs, health.CheckError{Subject: classCPU, Kind: "none", Err: errNoCPUSensor})
	}
	c.apply(res, now, sourceHwmon, err == nil, rd.obs, rd.retired, c.chipPresent(rd))

	return errs
}

// applyGPUs judges one GPU read and returns errs with its error added. The
// caller holds the lock.
func (c *Check) applyGPUs(res *health.CheckResult, now time.Time, gpus []GPU, err error, errs []error) []error {
	var obs []observed
	switch {
	case errors.Is(err, ErrNoGPUs):
		obs, err = c.stoppedGPUs(), nil
	case err != nil:
		errs = append(errs, fmt.Errorf("read gpus: %w", err))
	default:
		obs = c.gpuObserved(gpus)
	}
	c.apply(res, now, sourceGPU, err == nil, obs, nil, nil)

	return errs
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// chipDir identifies one hwmon chip directory by its chip name (sanitized,
// as in a condition key) and device path.
type chipDir struct{ chip, device string }

// hwmonRead is one hwmon read turned into observations.
type hwmonRead struct {
	obs []observed
	// retired maps the keys of sensors that are not judged (CPU cores,
	// Tccd<n>, a Tctl beside a Tdie, sample-only board sensors) to why. 001
	// judged them, so a seen one is forgotten, with an event only when it
	// leaves an open problem behind.
	retired map[string]string
	// hasCPU reports a CPU package sensor, readable or not.
	hasCPU bool
	// dirs and chips are the chip directories and chip names in the read.
	dirs  map[chipDir]bool
	chips map[string]bool
}

// chipPresent returns whether the chip of a seen hwmon key is in the read:
// its chip directory when its device is known, else any chip of that name (a
// key restored from 001).
func (c *Check) chipPresent(rd hwmonRead) func(key string) bool {
	return func(key string) bool {
		parts := strings.SplitN(key, "/", 3)
		if len(parts) < 3 {
			return false
		}
		if dev := c.devices[key]; dev != "" {
			return rd.dirs[chipDir{parts[1], dev}]
		}

		return rd.chips[parts[1]]
	}
}

// cpuInstance identifies one CPU chip instance (one package or die).
type cpuInstance struct{ chip, device string }

func isCPUChip(chip string) bool {
	switch chip {
	case "coretemp", "k10temp", "zenpower":
		return true
	}

	return false
}

// isPackageSensor reports whether s is the one judged sensor of its CPU chip
// instance: coretemp "Package id <n>"; for k10temp and zenpower the label
// packageLabels chose.
func isPackageSensor(s Sensor, pkg map[cpuInstance]string) bool {
	if s.Chip == "coretemp" {
		return strings.HasPrefix(s.Label, "Package id ")
	}
	label, ok := pkg[cpuInstance{s.Chip, s.Device}]

	return ok && s.Label == label
}

// cpuLabels are the package sensor candidates of one k10temp or zenpower
// chip instance.
type cpuLabels struct {
	tdie, tctl bool
	unlabeled  []string
}

// add notes one input label of the instance.
func (l *cpuLabels) add(label string) {
	switch {
	case label == "Tdie":
		l.tdie = true
	case label == "Tctl":
		l.tctl = true
	case isUnlabeled(label):
		l.unlabeled = append(l.unlabeled, label)
	}
}

// choose returns the label of the package sensor: "Tdie", else "Tctl", else
// the sole unlabeled input (a pre-Zen part exposes only temp1).
func (l *cpuLabels) choose() (string, bool) {
	switch {
	case l.tdie:
		return "Tdie", true
	case l.tctl:
		return "Tctl", true
	case len(l.unlabeled) == 1:
		return l.unlabeled[0], true
	}

	return "", false
}

// packageLabels returns, per k10temp or zenpower chip instance, the label of
// its package sensor (cpuLabels.choose). An instance with none has no entry.
func packageLabels(sensors []Sensor) map[cpuInstance]string {
	per := map[cpuInstance]*cpuLabels{}
	for _, s := range sensors {
		if s.Chip != "k10temp" && s.Chip != "zenpower" {
			continue
		}
		inst := cpuInstance{s.Chip, s.Device}
		if per[inst] == nil {
			per[inst] = &cpuLabels{}
		}
		per[inst].add(s.Label)
	}
	out := make(map[cpuInstance]string, len(per))
	for inst, l := range per {
		if label, ok := l.choose(); ok {
			out[inst] = label
		}
	}

	return out
}

// isUnlabeled reports whether label is the "temp<n>" name hwmon gives an
// input without a temp<n>_label file (or no label at all).
func isUnlabeled(label string) bool {
	n, ok := strings.CutPrefix(label, "temp")
	if !ok {
		return label == ""
	}
	_, err := strconv.Atoi(n)

	return err == nil
}

// names returns the condition key, sample metric and display name of a sensor
// of class (classCPU or classBoard). used counts the keys handed out so far; a
// repeated key gets a "-<n>" suffix.
func names(s Sensor, class string, used map[string]int) (key, metric, name string) {
	label := s.Label
	if label == "" {
		label = "temp"
	}
	key = "temps/" + sanitize(s.Chip) + "/" + sanitize(label)
	metric = "temp." + class + "." + sanitize(s.Chip) + "." + sanitize(label)
	name = s.Chip + " " + label
	used[key]++
	if n := used[key]; n > 1 {
		key += "-" + strconv.Itoa(n)
		metric += "-" + strconv.Itoa(n)
		name += " #" + strconv.Itoa(n)
	}

	return key, metric, name
}

// boardLimit returns the limit a board sensor is judged against: that of the
// first matching board_sensors entry, else the sensor's own critical limit
// (temp<n>_crit, when within (0, 150] and not the unset marker 127). ok is
// false for an unlisted sensor without one: it is sampled only.
func (c *Check) boardLimit(s Sensor) (limit float64, ok bool) {
	for _, b := range c.board {
		if !globMatch(b.Chip, s.Chip) || (b.Label != "" && !globMatch(b.Label, s.Label)) {
			continue
		}
		if b.Limit == nil {
			return float64(c.limits.Board), true
		}

		return float64(*b.Limit), true
	}
	if s.Crit != nil && *s.Crit > 0 && *s.Crit <= maxPlausible && *s.Crit != unsetCrit {
		return *s.Crit, true
	}

	return 0, false
}

// globMatch reports whether name matches the "*" glob pattern. The patterns
// are validated with the configuration, so a malformed one never matches.
func globMatch(pattern, name string) bool {
	ok, err := path.Match(pattern, name)

	return err == nil && ok
}

// treatment is what the check does with one hwmon sensor.
type treatment int

const (
	judgedSensor  treatment = iota // judged against a limit and sampled
	sampledSensor                  // sampled only: an unlisted board sensor without a critical limit
	ignoredSensor                  // neither: a CPU input other than the package sensor
)

// classify returns the class (classCPU or classBoard), limit and treatment of a
// sensor that is not a drive.
func (c *Check) classify(s Sensor, pkg map[cpuInstance]string) (class string, limit float64, how treatment) {
	if isCPUChip(s.Chip) {
		if !isPackageSensor(s, pkg) {
			return classCPU, 0, ignoredSensor
		}

		return classCPU, float64(c.limits.CPU), judgedSensor
	}
	if limit, ok := c.boardLimit(s); ok {
		return classBoard, limit, judgedSensor
	}

	return classBoard, 0, sampledSensor
}

// hwmonObserved judges one sensor per CPU package and the board sensors that
// are listed or expose a critical limit. A judged sensor that cannot be read
// is observed as unreadable; one that is sampled only is skipped.
// Sensors are ordered by device path first, and every sensor takes its key
// slot whether readable or not, so a second package keeps its "-2" suffix
// whatever order hwmon lists the chips in and whichever input fails.
func (c *Check) hwmonObserved(sensors []Sensor) hwmonRead {
	sensors = slices.Clone(sensors)
	slices.SortStableFunc(sensors, func(a, b Sensor) int { return strings.Compare(a.Device, b.Device) })
	pkg := packageLabels(sensors)

	rd := hwmonRead{
		obs:     make([]observed, 0, len(sensors)),
		retired: map[string]string{},
		dirs:    map[chipDir]bool{},
		chips:   map[string]bool{},
	}
	used := map[string]int{}
	for _, s := range sensors {
		rd.dirs[chipDir{sanitize(s.Chip), s.Device}] = true
		rd.chips[sanitize(s.Chip)] = true
		if s.Chip == "nvme" || s.Chip == "drivetemp" {
			continue
		}
		class, limit, how := c.classify(s, pkg)
		key, metric, name := names(s, class, used)
		rd.note(key, class, how)
		if how == ignoredSensor || (how == sampledSensor && s.Err != nil) {
			continue
		}
		o := observed{
			key: key, name: name, metric: metric, celsius: s.Celsius, limit: limit,
			sampleOnly: how == sampledSensor, device: s.Device,
		}
		rd.obs = append(rd.obs, withErr(o, s.Err))
	}

	return rd
}

// note records a CPU package sensor in hasCPU and a sensor that is not judged
// in retired.
func (rd *hwmonRead) note(key, class string, how treatment) {
	switch {
	case how == judgedSensor:
		rd.hasCPU = rd.hasCPU || class == classCPU
	case class == classCPU:
		rd.retired[key] = retiredCPU
	default:
		rd.retired[key] = retiredBoard
	}
}

// withErr marks o unreadable when its input could not be read.
func withErr(o observed, err error) observed {
	if err != nil {
		o.unreadable = fmt.Sprintf("%s cannot be read: %v", o.name, err)
	}

	return o
}

func gpuNames(idx int) (key, metric, name string) {
	n := strconv.Itoa(idx)

	return "temps/gpu/" + n, "temp.gpu." + n, "gpu " + n
}

// gpuObserved observes every GPU reported; one without a temperature is
// unreadable.
func (c *Check) gpuObserved(gpus []GPU) []observed {
	out := make([]observed, 0, len(gpus))
	for _, g := range gpus {
		key, metric, name := gpuNames(g.Index)
		o := observed{key: key, name: name, metric: metric, limit: float64(c.limits.GPU)}
		if g.Celsius == nil {
			o.unreadable = name + " reports no temperature"
		} else {
			o.celsius = *g.Celsius
		}
		out = append(out, o)
	}

	return out
}

// stoppedGPUs observes every GPU seen before as unreadable: the driver now
// reports no devices, so they stopped reporting rather than left.
func (c *Check) stoppedGPUs() []observed {
	prev := c.seen[sourceGPU]
	out := make([]observed, 0, len(prev))
	for _, key := range slices.Sorted(maps.Keys(prev)) {
		out = append(out, observed{
			key: key, name: prev[key], unreadable: prev[key] + " is not reported: the driver reports no devices",
		})
	}

	return out
}

// apply turns one source's observations into readings. ok is false when the
// source read failed: it emits no readings (Err reports it, states are kept)
// and nothing is "gone". A judged sensor that gives no reading, or a value
// outside the plausible range, is an Unknown reading and is not sampled; one
// at or above its limit is Bad. Seen keys missing from the read are handled
// by missing.
func (c *Check) apply(
	res *health.CheckResult, now time.Time, src source, ok bool, obs []observed,
	retired map[string]string, chipPresent func(key string) bool,
) {
	if !ok {
		return
	}
	cur := make(map[string]string, len(obs))
	infos := make(map[string]SensorStatus, len(obs))
	for _, o := range obs {
		infos[o.key] = c.statusOf(src, o)
		readable := o.unreadable == "" && o.celsius >= minPlausible && o.celsius <= maxPlausible
		if readable {
			res.Samples = append(res.Samples, health.Sample{Metric: o.metric, Value: o.celsius})
		}
		if o.sampleOnly {
			continue
		}
		cur[o.key] = o.name
		if src == sourceHwmon {
			c.devices[o.key] = o.device
		}
		r := judge(o)
		res.Readings = append(res.Readings, r)
		c.markUnreadable(o.key, r.Verdict == health.Unknown, now)
	}
	c.missing(res, now, c.seen[src], cur, retired, chipPresent)
	c.seen[src] = cur
	for key := range cur {
		if _, ok := infos[key]; !ok {
			infos[key] = c.info[src][key]
		}
	}
	c.info[src] = infos
}

// statusOf is the /temp status of one observation. A sensor observed without a
// metric (a GPU the driver stopped reporting) keeps what was shown of it before.
func (c *Check) statusOf(src source, o observed) SensorStatus {
	if prev, ok := c.info[src][o.key]; ok && o.metric == "" {
		return prev
	}
	label := strings.Replace(strings.TrimPrefix(o.key, "temps/"), "/", " ", 1)

	return SensorStatus{Metric: o.metric, Label: label, Limit: o.limit}
}

// missing handles the keys of prev absent from cur. Each is forgotten (listed
// in Forget) with an event only when its condition is open: a retired key
// with why it is no longer judged, a key whose chip left the read as "gone".
// A key whose chip is still in the read (chipPresent, nil for GPUs) lost only
// its input: with an open problem it is kept in cur as unreadable, else
// forgotten silently.
func (c *Check) missing(
	res *health.CheckResult, now time.Time, prev, cur, retired map[string]string,
	chipPresent func(key string) bool,
) {
	for _, key := range slices.Sorted(maps.Keys(prev)) {
		if _, still := cur[key]; still {
			continue
		}
		name, open := prev[key], c.open.IsOpen(key)
		why, isRetired := retired[key]
		switch {
		case isRetired:
			if open {
				res.Readings = append(res.Readings, event(key, name+why))
			}
		case chipPresent != nil && chipPresent(key):
			if open {
				cur[key] = name
				res.Readings = append(res.Readings, health.Reading{
					Key: key, Verdict: health.Unknown, Kind: health.State,
					Text: name + " is no longer reported by its chip",
				})
				c.markUnreadable(key, true, now)

				continue
			}
		case open:
			res.Readings = append(res.Readings, event(key, name+" gone"))
		}
		res.Forget = append(res.Forget, key)
		delete(c.devices, key)
		delete(c.since, key)
	}
}

// markUnreadable records since when key gives no reading, or clears it.
func (c *Check) markUnreadable(key string, unreadable bool, now time.Time) {
	if !unreadable {
		delete(c.since, key)

		return
	}
	if _, ok := c.since[key]; !ok {
		c.since[key] = now
	}
}

func event(key, text string) health.Reading {
	return health.Reading{Key: key, Verdict: health.Good, Text: text, Kind: health.Event}
}

// judge turns one judged observation into its State reading.
func judge(o observed) health.Reading {
	r := health.Reading{
		Key: o.key, Verdict: health.Good, Kind: health.State, Text: fmt.Sprintf("%s at %.1f°C", o.name, o.celsius),
	}
	switch {
	case o.unreadable != "":
		r.Verdict = health.Unknown
		r.Text = o.unreadable
	case o.celsius < minPlausible || o.celsius > maxPlausible:
		r.Verdict = health.Unknown
		r.Text = fmt.Sprintf("%s reads %.1f°C, outside the plausible %g…%g°C range",
			o.name, o.celsius, minPlausible, maxPlausible)
	case o.limit > 0 && o.celsius >= o.limit:
		r.Verdict = health.Bad
		r.Text = fmt.Sprintf("%s at %.1f°C, at or above the %s°C limit",
			o.name, o.celsius, strconv.FormatFloat(o.limit, 'f', -1, 64))
	}

	return r
}

// sanitize lowercases s and maps everything outside [a-z0-9._-] to '-'.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}

	return b.String()
}
