// Package nvidia reads GPU temperatures through nvidia-smi.
package nvidia

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strconv"
	"strings"
)

const defaultBinary = "nvidia-smi"

// ErrNoDevices is returned by GPUs when nvidia-smi reports "No devices were found"
// (driver present, no GPU visible). An absent nvidia-smi is not this error.
var ErrNoDevices = errors.New("nvidia-smi: no devices were found")

// GPU is one GPU and its core temperature. Celsius is nil when nvidia-smi reports
// the temperature as unavailable ("[N/A]").
type GPU struct {
	Index   int
	Name    string
	Celsius *float64
}

// Reader runs nvidia-smi.
type Reader struct {
	binary string
}

// New creates a Reader for the given nvidia-smi binary (name or path).
func New(binary string) *Reader {
	return &Reader{binary: binary}
}

// NewDefault creates a Reader that looks up nvidia-smi on PATH.
func NewDefault() *Reader {
	return New(defaultBinary)
}

// GPUs queries every GPU. An absent binary means no GPUs and no error; nvidia-smi
// reporting that no devices were found returns ErrNoDevices.
func (r *Reader) GPUs(ctx context.Context) ([]GPU, error) {
	path, err := exec.LookPath(r.binary)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("locate %s: %w", r.binary, err)
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(
		ctx,
		path,
		"--query-gpu=index,name,temperature.gpu",
		"--format=csv,noheader,nounits",
	)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if noDevices(err, stdout.String(), stderr.String()) {
			return nil, ErrNoDevices
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("run %s: %w: %s", r.binary, err, msg)
		}

		return nil, fmt.Errorf("run %s: %w", r.binary, err)
	}

	return Parse(stdout.String())
}

// noDevices reports whether a failed run is nvidia-smi's "No devices were found"
// outcome (driver present, no GPU visible): a non-zero exit status together with
// that message on stdout or stderr, matched case-insensitively.
func noDevices(err error, stdout, stderr string) bool {
	if _, ok := errors.AsType[*exec.ExitError](err); !ok {
		return false
	}

	const marker = "no devices were found"

	return strings.Contains(strings.ToLower(stdout), marker) || strings.Contains(strings.ToLower(stderr), marker)
}

// Parse parses nvidia-smi CSV output ("index, name, temperature"). The name may
// itself contain commas: the first field is the index and the last the temperature.
func Parse(out string) ([]GPU, error) {
	var gpus []GPU
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		first := strings.Index(line, ",")
		last := strings.LastIndex(line, ",")
		if first < 0 || first == last {
			return nil, fmt.Errorf("malformed nvidia-smi line %q", line)
		}
		idx, err := strconv.Atoi(strings.TrimSpace(line[:first]))
		if err != nil {
			return nil, fmt.Errorf("parse GPU index in %q: %w", line, err)
		}
		name := strings.TrimSpace(line[first+1 : last])
		field := strings.TrimSpace(line[last+1:])
		if strings.HasPrefix(field, "[") {
			// "[N/A]" / "[Not Supported]": the GPU is present but has no reading.
			gpus = append(gpus, GPU{Index: idx, Name: name})
			continue
		}

		temp, err := strconv.ParseFloat(field, 64)
		if err != nil {
			return nil, fmt.Errorf("parse GPU temperature in %q: %w", line, err)
		}
		gpus = append(gpus, GPU{Index: idx, Name: name, Celsius: &temp})
	}

	return gpus, nil
}
