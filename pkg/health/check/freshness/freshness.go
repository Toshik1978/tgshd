// Package freshness implements the file-age check: a file holding a Unix
// time (or the file's modification time) must be newer than a maximum age.
package freshness

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/config"
)

const (
	kvPrefix = "freshness."
	kvSuffix = ".first_missing"

	// stampSuffix and readOKSuffix key the data the weekly summary shows: the
	// file's timestamp (Unix seconds) and whether the last run could read it.
	stampSuffix  = ".stamp"
	readOKSuffix = ".read_ok"
)

// Source is the file access the check needs. Implementations must return
// errors that satisfy errors.Is(err, fs.ErrNotExist) for a missing file.
type Source interface {
	ModTime(path string) (time.Time, error)
	ReadFile(path string) ([]byte, error)
}

// State persists, per entry, the time a file was first seen missing and the
// stamp the last run saw, for the weekly summary.
type State interface {
	// RecordStamp stores the file's timestamp and marks the read as ok.
	RecordStamp(name string, stamp time.Time)
	// RecordNoStamp drops the stored timestamp: readOK is true when the file
	// is missing and false when it could not be read.
	RecordNoStamp(name string, readOK bool)

	FirstMissing(name string) (time.Time, bool)
	SetFirstMissing(name string, t time.Time)
	ClearFirstMissing(name string)
}

// KVState is a State backed by a string map shaped like the store's KV map.
// Entry values live under "freshness.<name>.first_missing" as Unix seconds;
// "freshness.<name>.stamp" holds the file's timestamp (Unix seconds, absent
// when missing or unreadable) and "freshness.<name>.read_ok" is "1" or "0".
// The runner builds it with NewKVState from the loaded KV and persists
// Export() back after each run. It is safe for concurrent use.
type KVState struct {
	mu sync.Mutex
	kv map[string]string
}

// NewKVState restores the state from kv, keeping only freshness keys. A nil
// map starts empty.
func NewKVState(kv map[string]string) *KVState {
	s := &KVState{kv: make(map[string]string)}
	for k, v := range kv {
		if strings.HasPrefix(k, kvPrefix) && isStateKey(k) {
			s.kv[k] = v
		}
	}

	return s
}

func isStateKey(k string) bool {
	return strings.HasSuffix(k, kvSuffix) || strings.HasSuffix(k, stampSuffix) || strings.HasSuffix(k, readOKSuffix)
}

// Export returns a copy of the freshness keys, to merge into the persisted KV.
func (s *KVState) Export() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make(map[string]string, len(s.kv))
	maps.Copy(out, s.kv)

	return out
}

// FirstMissing returns the recorded time; a corrupt value reads as absent.
func (s *KVState) FirstMissing(name string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sec, err := strconv.ParseInt(s.kv[key(name)], 10, 64)
	if err != nil {
		return time.Time{}, false
	}

	return time.Unix(sec, 0), true
}

// SetFirstMissing records t for the entry.
func (s *KVState) SetFirstMissing(name string, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.kv[key(name)] = strconv.FormatInt(t.Unix(), 10)
}

// ClearFirstMissing forgets the entry's mark.
func (s *KVState) ClearFirstMissing(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.kv, key(name))
}

// RecordStamp stores the entry's file timestamp and a successful read.
func (s *KVState) RecordStamp(name string, stamp time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.kv[kvPrefix+name+stampSuffix] = strconv.FormatInt(stamp.Unix(), 10)
	s.kv[kvPrefix+name+readOKSuffix] = "1"
}

// RecordNoStamp forgets the entry's timestamp and records whether the file
// was readable (missing) or not.
func (s *KVState) RecordNoStamp(name string, readOK bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.kv, kvPrefix+name+stampSuffix)
	s.kv[kvPrefix+name+readOKSuffix] = "0"
	if readOK {
		s.kv[kvPrefix+name+readOKSuffix] = "1"
	}
}

// Stamp returns the entry's last seen timestamp: present is false when it is
// missing or corrupt, readOK is true when the last run could read the file.
func (s *KVState) Stamp(name string) (stamp time.Time, present, readOK bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	readOK = s.kv[kvPrefix+name+readOKSuffix] == "1"
	sec, err := strconv.ParseInt(s.kv[kvPrefix+name+stampSuffix], 10, 64)
	if err != nil {
		return time.Time{}, false, readOK
	}

	return time.Unix(sec, 0), true, readOK
}

func key(name string) string { return kvPrefix + name + kvSuffix }

// Check judges one configured freshness entry.
type Check struct {
	cfg   config.Freshness
	src   Source
	state State
	now   func() time.Time
}

// New builds the check for one entry. now is the injected clock.
func New(cfg config.Freshness, src Source, state State, now func() time.Time) *Check {
	return &Check{cfg: cfg, src: src, state: state, now: now}
}

// Run reads the file's timestamp and judges it against max_age.
//
// A missing file yields no reading until max_age has passed since the first
// run that saw it missing (not broken yet, and Good would falsely resolve an
// open stale-file problem), then Bad. Any other read or parse failure is Unknown and leaves
// the first-missing mark untouched.
func (c *Check) Run(_ context.Context) health.CheckResult {
	now := c.now()
	reading := health.Reading{Key: "freshness/" + c.cfg.Name, Kind: health.State}

	stamp, err := c.timestamp()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		var judged bool

		c.state.RecordNoStamp(c.cfg.Name, true)
		if reading, judged = c.judgeMissing(reading, now); !judged {
			return health.CheckResult{}
		}
	case err != nil:
		c.state.RecordNoStamp(c.cfg.Name, false)
		reading.Verdict = health.Unknown
		reading.Text = fmt.Sprintf("freshness %s: cannot read %s: %v", c.cfg.Name, c.cfg.Path, err)
	default:
		c.state.ClearFirstMissing(c.cfg.Name)
		c.state.RecordStamp(c.cfg.Name, stamp)
		reading = c.judgeAge(reading, now.Sub(stamp))
	}

	return health.CheckResult{Readings: []health.Reading{reading}}
}

func (c *Check) timestamp() (time.Time, error) {
	if c.cfg.Content == config.ContentMtime {
		t, err := c.src.ModTime(c.cfg.Path)
		if err != nil {
			return time.Time{}, fmt.Errorf("stat %s: %w", c.cfg.Path, err)
		}
		return t, nil
	}
	data, err := c.src.ReadFile(c.cfg.Path)
	if err != nil {
		return time.Time{}, fmt.Errorf("read %s: %w", c.cfg.Path, err)
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse epoch in %s: %w", c.cfg.Path, err)
	}

	return time.Unix(sec, 0), nil
}

func (c *Check) judgeAge(r health.Reading, age time.Duration) health.Reading {
	maxAge := c.cfg.MaxAge.Duration()
	if age > maxAge {
		r.Verdict = health.Bad
		r.Text = fmt.Sprintf("freshness %s: %s is %s old (max %s)",
			c.cfg.Name, c.cfg.Path, age.Round(time.Second), maxAge)
		return r
	}
	r.Verdict = health.Good
	r.Text = fmt.Sprintf("freshness %s: %s is fresh", c.cfg.Name, c.cfg.Path)

	return r
}

// judgeMissing reports false while the file is missing within the grace
// window: the check then emits no reading, so an open problem (for example a
// stale file that was deleted) is neither resolved nor re-raised.
func (c *Check) judgeMissing(r health.Reading, now time.Time) (health.Reading, bool) {
	first, ok := c.state.FirstMissing(c.cfg.Name)
	if !ok {
		first = now
		c.state.SetFirstMissing(c.cfg.Name, now)
	}

	// The stored mark has second precision; compare at the same precision.
	maxAge := c.cfg.MaxAge.Duration()
	if time.Duration(now.Unix()-first.Unix())*time.Second <= maxAge {
		return r, false
	}

	r.Verdict = health.Bad
	r.Text = fmt.Sprintf("freshness %s: %s missing for more than %s", c.cfg.Name, c.cfg.Path, maxAge)

	return r, true
}
