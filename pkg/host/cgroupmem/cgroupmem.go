// Package cgroupmem reads per-service memory from cgroup v2 and can kill a
// whole service cgroup through cgroup.kill.
package cgroupmem

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	defaultRoot     = "/sys/fs/cgroup"
	defaultProcRoot = "/proc"
	systemSlice     = "system.slice"
)

// Kind tells a systemd unit from a docker container.
type Kind int

const (
	// KindUnit is a systemd service (*.service).
	KindUnit Kind = iota
	// KindContainer is a docker container (docker-<id>.scope).
	KindContainer
)

// Service is one eligible cgroup under system.slice.
type Service struct {
	Kind    Kind
	Name    string // unit name, or the full container id for containers
	Path    string // cgroup path relative to the cgroup root
	Anon    uint64 // bytes: memory.stat anon + shmem
	Current uint64 // bytes: memory.current
}

// Reader reads cgroup v2 memory facts.
type Reader struct {
	root     string
	procRoot string
}

// New creates a Reader over a cgroup v2 root and a procfs-style root.
func New(root, procRoot string) *Reader {
	return &Reader{root: root, procRoot: procRoot}
}

// NewDefault creates a Reader over the real /sys/fs/cgroup and /proc.
func NewDefault() *Reader {
	return New(defaultRoot, defaultProcRoot)
}

// Services lists *.service and docker-<id>.scope cgroups directly under
// system.slice and one level down in nested *.slice directories. Cgroups that
// vanish while being read are skipped.
func (r *Reader) Services() ([]Service, error) {
	entries, err := os.ReadDir(filepath.Join(r.root, systemSlice))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", systemSlice, err)
	}
	var out []Service
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".slice") {
			if out, err = r.appendService(out, systemSlice, e.Name()); err != nil {
				return nil, err
			}

			continue
		}
		if out, err = r.appendNested(out, filepath.Join(systemSlice, e.Name())); err != nil {
			return nil, err
		}
	}

	return out, nil
}

// anonBytes returns anon + shmem from memory.stat content; anon must be present.
func anonBytes(stat []byte) (uint64, error) {
	var total uint64
	var haveAnon bool
	for line := range bytes.SplitSeq(stat, []byte{'\n'}) {
		key, val, _ := bytes.Cut(line, []byte{' '})
		if string(key) != "anon" && string(key) != "shmem" {
			continue
		}
		v, err := strconv.ParseUint(string(bytes.TrimSpace(val)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", key, err)
		}
		total += v
		haveAnon = haveAnon || string(key) == "anon"
	}
	if !haveAnon {
		return 0, errors.New("anon not found")
	}

	return total, nil
}

// Self returns tgshd's own cgroup path (the "0::" line of /proc/self/cgroup),
// relative to the cgroup root like Service.Path.
func (r *Reader) Self() (string, error) {
	data, err := os.ReadFile(filepath.Join(r.procRoot, "self", "cgroup"))
	if err != nil {
		return "", fmt.Errorf("read self cgroup: %w", err)
	}
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		if path, ok := bytes.CutPrefix(line, []byte("0::")); ok {
			return strings.TrimPrefix(string(bytes.TrimSpace(path)), "/"), nil
		}
	}

	return "", errors.New("read self cgroup: no cgroup v2 entry")
}

// Kill writes 1 to <root>/<path>/cgroup.kill, killing every process of the
// cgroup. Empty paths and paths escaping the root are refused.
func (r *Reader) Kill(path string) error {
	if path == "" || filepath.Clean(path) == "." || !filepath.IsLocal(path) {
		return fmt.Errorf("kill cgroup: invalid path %q", path)
	}
	f, err := os.OpenFile(filepath.Join(r.root, path, "cgroup.kill"), os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("kill cgroup %s: %w", path, err)
	}
	if _, err := f.WriteString("1"); err != nil {
		_ = f.Close()

		return fmt.Errorf("kill cgroup %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("kill cgroup %s: %w", path, err)
	}

	return nil
}

func (r *Reader) appendNested(out []Service, slice string) ([]Service, error) {
	nested, err := os.ReadDir(filepath.Join(r.root, slice))
	if isGone(err) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", slice, err)
	}
	for _, n := range nested {
		if !n.IsDir() {
			continue
		}
		if out, err = r.appendService(out, slice, n.Name()); err != nil {
			return nil, err
		}
	}

	return out, nil
}

func (r *Reader) appendService(out []Service, parent, name string) ([]Service, error) {
	svc := Service{Path: filepath.Join(parent, name)}
	switch {
	case strings.HasSuffix(name, ".service"):
		svc.Kind, svc.Name = KindUnit, name
	case strings.HasPrefix(name, "docker-") && strings.HasSuffix(name, ".scope"):
		svc.Kind, svc.Name = KindContainer, strings.TrimSuffix(strings.TrimPrefix(name, "docker-"), ".scope")
	default:
		return out, nil
	}
	anon, current, err := r.readMemory(svc.Path)
	if isGone(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	svc.Anon, svc.Current = anon, current

	return append(out, svc), nil
}

func (r *Reader) readMemory(path string) (anon, current uint64, err error) {
	dir := filepath.Join(r.root, path)
	stat, err := os.ReadFile(filepath.Join(dir, "memory.stat"))
	if err != nil {
		return 0, 0, fmt.Errorf("read %s memory.stat: %w", path, err)
	}
	if anon, err = anonBytes(stat); err != nil {
		return 0, 0, fmt.Errorf("parse %s memory.stat: %w", path, err)
	}
	cur, err := os.ReadFile(filepath.Join(dir, "memory.current"))
	if err != nil {
		return 0, 0, fmt.Errorf("read %s memory.current: %w", path, err)
	}
	if current, err = strconv.ParseUint(string(bytes.TrimSpace(cur)), 10, 64); err != nil {
		return 0, 0, fmt.Errorf("parse %s memory.current: %w", path, err)
	}

	return anon, current, nil
}

// isGone reports whether err means the cgroup was removed while being read:
// ENOENT, or ENODEV/ESRCH which cgroupfs returns for a removed directory.
func isGone(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENODEV) || errors.Is(err, syscall.ESRCH)
}
