package runner

import (
	"context"
	"errors"
	"slices"
	"strings"

	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/notify"
	"github.com/Toshik1978/tgshd/pkg/health/store"
)

// MinSerialSuffix is the shortest suffix of a serial /retire accepts.
const MinSerialSuffix = 4

const (
	retireCheck     = "disks"
	retirePrefix    = retireCheck + "/"
	serialTailChars = 4
)

var (
	// ErrNoDrive means no known drive matches the given serial.
	ErrNoDrive = errors.New("no known drive matches")
	// ErrAmbiguousDrive means more than one known drive matches the given suffix.
	ErrAmbiguousDrive = errors.New("more than one known drive matches")
)

// DriveBook holds the drive baselines; *disks.Check satisfies it.
type DriveBook interface {
	Baselines() []store.Baseline
	Retire(serial string) bool
}

// Drive identifies a drive in replies.
type Drive struct {
	Serial string
	Model  string
	Bus    string
}

// Label is the drive as shown to people: bus, model and the last four
// characters of the serial, never the full serial.
func (d Drive) Label() string {
	r := []rune(d.Serial)
	tail := string(r[max(len(r)-serialTailChars, 0):])
	parts := make([]string, 0, 3)
	for _, p := range []string{d.Bus, d.Model} {
		if p != "" {
			parts = append(parts, p)
		}
	}

	return strings.Join(append(parts, "…"+tail), " ")
}

// RetireResult is the outcome of Retire. Drive and Cleared are set on
// success; Candidates lists the drives to choose from when the match failed.
type RetireResult struct {
	Drive      Drive
	Cleared    int
	Candidates []Drive
}

// Retire retires the active drive whose serial equals serial, or else the one
// whose serial ends with it (at least MinSerialSuffix characters, matched
// case-insensitively). It clears the drive's disks/<serial>/ conditions
// without any RESOLVED message, records a retired alert and pushes one event
// message. With no match or several, it returns ErrNoDrive or
// ErrAmbiguousDrive and the candidates.
func (r *Runner) Retire(ctx context.Context, serial string) (RetireResult, error) {
	if r.opts.Drives == nil {
		return RetireResult{}, ErrNoDrive
	}
	drives := r.activeDrives()

	matches := matchDrives(drives, strings.TrimSpace(serial))
	switch {
	case len(matches) == 0:
		return RetireResult{Candidates: drives}, ErrNoDrive
	case len(matches) > 1:
		return RetireResult{Candidates: matches}, ErrAmbiguousDrive
	}
	d := matches[0]
	if !r.opts.Drives.Retire(d.Serial) {
		return RetireResult{Candidates: r.activeDrives()}, ErrNoDrive
	}
	prefix := retirePrefix + d.Serial + "/"
	cleared := r.engine.ClearPrefix(prefix)
	r.notifier.ForgetEnded(func(key string) bool { return strings.HasPrefix(key, prefix) })
	text := "Drive retired: " + d.Label()

	r.mu.Lock()
	r.logged = append(r.logged, store.Alert{
		At: r.opts.Now(), Kind: store.KindRetired, Check: retireCheck, Key: retirePrefix + d.Serial, Text: text,
	})
	r.mu.Unlock()

	err := r.notifier.Notify(ctx, notify.Message{
		Kind: notify.KindEvent, Check: retireCheck, Key: retirePrefix + d.Serial + "/retired", Text: text,
	})
	if err != nil {
		r.opts.Logger.Warn("cannot notify about a retired drive", zap.String("drive", d.Label()), zap.Error(err))
	}

	return RetireResult{Drive: d, Cleared: cleared}, nil
}

func (r *Runner) activeDrives() []Drive {
	var out []Drive
	bases := r.opts.Drives.Baselines()
	for i := range bases {
		if b := &bases[i]; !b.Retired {
			out = append(out, Drive{Serial: b.Serial, Model: b.Model, Bus: b.Bus})
		}
	}
	slices.SortFunc(out, func(a, b Drive) int { return strings.Compare(a.Serial, b.Serial) })

	return out
}

// matchDrives returns the exact match if there is one, else the drives whose
// serial ends with s, provided s is long enough.
func matchDrives(drives []Drive, s string) []Drive {
	if s == "" {
		return nil
	}
	for _, d := range drives {
		if strings.EqualFold(d.Serial, s) {
			return []Drive{d}
		}
	}
	if len([]rune(s)) < MinSerialSuffix {
		return nil
	}
	var out []Drive
	for _, d := range drives {
		if strings.HasSuffix(strings.ToLower(d.Serial), strings.ToLower(s)) {
			out = append(out, d)
		}
	}

	return out
}

// dropRetired removes the readings of a disks run that belong to a retired
// drive, so a result computed before /retire cannot recreate its conditions.
// A drive that reappears has a fresh baseline, so it is no longer retired.
func (r *Runner) dropRetired(check string, readings []health.Reading) []health.Reading {
	if check != retireCheck || r.opts.Drives == nil || len(readings) == 0 {
		return readings
	}
	var prefixes []string
	bases := r.opts.Drives.Baselines()
	for i := range bases {
		if bases[i].Retired {
			prefixes = append(prefixes, retirePrefix+bases[i].Serial+"/")
		}
	}
	if len(prefixes) == 0 {
		return readings
	}

	return slices.DeleteFunc(slices.Clone(readings), func(rd health.Reading) bool {
		return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(rd.Key, p) })
	})
}
