// Package temps judges CPU, board and GPU temperatures. Drive temperatures are
// judged by the disks check from SMART data.
package temps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/config"
)

const gpuTimeout = 30 * time.Second

// Sensor is one hwmon temperature input.
type Sensor struct {
	Chip    string
	Label   string
	Celsius float64
	Max     *float64
	Crit    *float64
}

// GPU is one NVIDIA GPU temperature.
type GPU struct {
	Index   int
	Name    string
	Celsius float64
}

// Hwmon reads the hwmon sensors.
type Hwmon interface {
	Sensors() ([]Sensor, error)
}

// Nvidia reads the NVIDIA GPU temperatures.
type Nvidia interface {
	GPUs(ctx context.Context) ([]GPU, error)
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

	mu   sync.Mutex
	seen [2]map[string]string // per source: condition key -> display name
}

// New returns a temperatures check. Either source may be nil.
func New(hwmon Hwmon, nvidia Nvidia, limits config.Limits) *Check {
	return &Check{
		hwmon:  hwmon,
		nvidia: nvidia,
		limits: limits,
		seen:   [2]map[string]string{{}, {}},
	}
}

type observed struct {
	key, name, metric string
	celsius           float64
	limit             int
}

// Run reads every source and judges each sensor against its class limit.
func (c *Check) Run(ctx context.Context) health.CheckResult {
	c.mu.Lock()
	defer c.mu.Unlock()

	var (
		res  health.CheckResult
		errs []error
	)
	if c.hwmon != nil {
		sensors, err := c.hwmon.Sensors()
		if err != nil {
			errs = append(errs, fmt.Errorf("read hwmon: %w", err))
		}
		c.apply(&res, sourceHwmon, err == nil, c.hwmonObserved(sensors))
	}
	if c.nvidia != nil {
		gctx, cancel := context.WithTimeout(ctx, gpuTimeout)
		gpus, err := c.nvidia.GPUs(gctx)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("read gpus: %w", err))
		}
		c.apply(&res, sourceGPU, err == nil, c.gpuObserved(gpus))
	}
	res.Err = errors.Join(errs...)

	return res
}

type seenState struct {
	Hwmon map[string]string `json:"hwmon"`
	GPU   map[string]string `json:"gpu"`
}

// Export serialises the set of known sensors, to be kept in the store so that
// gone detection survives a restart.
func (c *Check) Export() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := json.Marshal(seenState{Hwmon: c.seen[sourceHwmon], GPU: c.seen[sourceGPU]})
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

	return nil
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func (c *Check) hwmonObserved(sensors []Sensor) []observed {
	out := make([]observed, 0, len(sensors))
	used := map[string]int{}
	for _, s := range sensors {
		var class string
		var limit int
		switch s.Chip {
		case "nvme", "drivetemp":
			continue
		case "coretemp", "k10temp", "zenpower":
			class, limit = "cpu", c.limits.CPU
		default:
			class, limit = "board", c.limits.Board
		}
		label := s.Label
		if label == "" {
			label = "temp"
		}
		base := "temps/" + sanitize(s.Chip) + "/" + sanitize(label)
		metric := "temp.cpu." + sanitize(s.Chip) + "." + sanitize(label)
		if class == "board" {
			metric = "temp.board." + sanitize(s.Chip) + "." + sanitize(label)
		}
		name := s.Chip + " " + label
		used[base]++
		if n := used[base]; n > 1 {
			base += "-" + strconv.Itoa(n)
			metric += "-" + strconv.Itoa(n)
			name += " #" + strconv.Itoa(n)
		}
		out = append(out, observed{key: base, name: name, metric: metric, celsius: s.Celsius, limit: limit})
	}

	return out
}

func (c *Check) gpuObserved(gpus []GPU) []observed {
	out := make([]observed, 0, len(gpus))
	for _, g := range gpus {
		idx := strconv.Itoa(g.Index)
		out = append(out, observed{
			key:     "temps/gpu/" + idx,
			name:    "gpu " + idx,
			metric:  "temp.gpu." + idx,
			celsius: g.Celsius,
			limit:   c.limits.GPU,
		})
	}

	return out
}

// apply turns one source's observations into readings. ok is false when the
// source read failed: it emits no readings (Err reports it, states are kept) and nothing is "gone".
func (c *Check) apply(res *health.CheckResult, src source, ok bool, obs []observed) {
	prev := c.seen[src]
	if !ok {
		return
	}
	cur := make(map[string]string, len(obs))
	for _, o := range obs {
		cur[o.key] = o.name
		v, text := health.Good, fmt.Sprintf("%s at %.1f°C", o.name, o.celsius)
		if o.limit > 0 && o.celsius > float64(o.limit) {
			v, text = health.Bad, fmt.Sprintf("%s at %.1f°C, above the %d°C limit", o.name, o.celsius, o.limit)
		}
		res.Readings = append(res.Readings, health.Reading{Key: o.key, Verdict: v, Text: text, Kind: health.State})
		res.Samples = append(res.Samples, health.Sample{Metric: o.metric, Value: o.celsius})
	}
	for key, name := range prev {
		if _, still := cur[key]; still {
			continue
		}
		res.Readings = append(res.Readings,
			health.Reading{Key: key, Verdict: health.Good, Kind: health.State},
			health.Reading{Key: key, Verdict: health.Good, Text: name + " gone", Kind: health.Event},
		)
	}
	c.seen[src] = cur
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
