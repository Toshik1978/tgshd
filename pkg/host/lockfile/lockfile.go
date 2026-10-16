// Package lockfile provides flock-based locks and the heartbeat file used by
// the host health monitor.
package lockfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	dirMode  = 0o755
	fileMode = 0o644

	// MaxPlannedSeconds is the longest accepted planned-reboot window.
	MaxPlannedSeconds = 86400
)

var (
	// ErrAlreadyLocked is returned when another process holds the instance lock.
	ErrAlreadyLocked = errors.New("lock already held")
	// ErrCorrupt is wrapped by the error of a heartbeat that is not in the
	// file format.
	ErrCorrupt = errors.New("corrupt heartbeat")
)

// MaintenanceHeld reports whether another process holds the maintenance lock
// exclusively. It never holds the lock itself: a shared non-blocking lock is
// taken and released at once. A missing file means not held.
func MaintenanceHeld(path string) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open maintenance lock: %w", err)
	}
	defer func() { _ = f.Close() }()

	err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("probe maintenance lock: %w", err)
	}
	// Closing the file releases the lock.
	return false, nil
}

// Instance is an exclusive lock held for the life of the daemon.
type Instance struct {
	file *os.File
}

// AcquireInstance takes an exclusive non-blocking flock on path, creating the
// file and its directory when needed. It returns ErrAlreadyLocked if held.
func AcquireInstance(path string) (*Instance, error) {
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open instance lock: %w", err)
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		_ = f.Close()
		return nil, ErrAlreadyLocked
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock instance: %w", err)
	}

	return &Instance{file: f}, nil
}

// Release drops the lock. It is safe to call more than once.
func (i *Instance) Release() error {
	if i.file == nil {
		return nil
	}
	err := i.file.Close()
	i.file = nil
	if err != nil {
		return fmt.Errorf("release instance lock: %w", err)
	}

	return nil
}

// Heartbeat is the content of the heartbeat file; PlannedUntil 0 means none.
type Heartbeat struct {
	TS           int64
	PlannedUntil int64
}

// String renders the single-line file format.
func (h Heartbeat) String() string {
	return fmt.Sprintf("ts=%d planned_until=%d\n", h.TS, h.PlannedUntil)
}

// ParseHeartbeat parses "ts=<epoch> planned_until=<epoch>".
func ParseHeartbeat(content string) (Heartbeat, error) {
	var (
		h            Heartbeat
		hasTS, hasPU bool
	)
	for field := range strings.FieldsSeq(content) {
		key, val, ok := strings.Cut(field, "=")
		if !ok {
			return Heartbeat{}, fmt.Errorf("%w: bad field %q", ErrCorrupt, field)
		}
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return Heartbeat{}, fmt.Errorf("%w: field %s: %w", ErrCorrupt, key, err)
		}
		switch key {
		case "ts":
			h.TS, hasTS = n, true
		case "planned_until":
			h.PlannedUntil, hasPU = n, true
		}
	}
	if !hasTS || !hasPU {
		return Heartbeat{}, fmt.Errorf("%w: missing field", ErrCorrupt)
	}

	return h, nil
}

// ReadHeartbeat reads the heartbeat file. A missing file yields a zero value.
func ReadHeartbeat(path string) (Heartbeat, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Heartbeat{}, nil
	}
	if err != nil {
		return Heartbeat{}, fmt.Errorf("read heartbeat: %w", err)
	}

	return ParseHeartbeat(string(data))
}

// WriteTick writes ts=now, keeping planned_until until it has passed, then 0.
func WriteTick(path string, now time.Time) error {
	return update(path, func(h Heartbeat) Heartbeat {
		if h.PlannedUntil <= now.Unix() {
			h.PlannedUntil = 0
		}
		h.TS = now.Unix()
		return h
	})
}

// SetPlanned sets planned_until to now+seconds (0 clears it), preserving ts.
// A file that is missing, corrupt or has no ts yet gets ts=now, so a watchdog
// does not read it as stale before the daemon's first tick.
// seconds must be within 0..MaxPlannedSeconds.
func SetPlanned(path string, now time.Time, seconds int) error {
	if seconds < 0 || seconds > MaxPlannedSeconds {
		return fmt.Errorf("planned window %d s out of range 0..%d", seconds, MaxPlannedSeconds)
	}

	return update(path, func(h Heartbeat) Heartbeat {
		if h.TS == 0 {
			h.TS = now.Unix()
		}
		h.PlannedUntil = 0
		if seconds > 0 {
			h.PlannedUntil = now.Unix() + int64(seconds)
		}

		return h
	})
}

// update applies fn to the current heartbeat under the sidecar lock and
// atomically replaces the file. A missing or corrupt file counts as empty; a
// file that cannot be read is left as it is and the error returned, so a
// live planned_until is never dropped.
func update(path string, fn func(Heartbeat) Heartbeat) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("create heartbeat directory: %w", err)
	}

	lock, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, fileMode)
	if err != nil {
		return fmt.Errorf("open heartbeat lock: %w", err)
	}
	defer func() { _ = lock.Close() }()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock heartbeat: %w", err)
	}

	current, err := ReadHeartbeat(path)
	switch {
	case errors.Is(err, ErrCorrupt):
		current = Heartbeat{}
	case err != nil:
		return fmt.Errorf("heartbeat not rewritten: %w", err)
	}

	return writeAtomic(dir, path, fn(current).String())
}

func writeAtomic(dir, path, content string) (err error) {
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create heartbeat temp file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write heartbeat: %w", err)
	}
	if err = tmp.Chmod(fileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod heartbeat: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close heartbeat: %w", err)
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("rename heartbeat: %w", err)
	}

	return nil
}
