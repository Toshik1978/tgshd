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

// GPU is one GPU and its core temperature.
type GPU struct {
	Index   int
	Name    string
	Celsius float64
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

// GPUs queries every GPU. An absent binary means no GPUs and no error.
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
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("run %s: %w: %s", r.binary, err, msg)
		}

		return nil, fmt.Errorf("run %s: %w", r.binary, err)
	}

	return Parse(stdout.String())
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
		field := strings.TrimSpace(line[last+1:])
		if strings.HasPrefix(field, "[") {
			continue // "[N/A]" / "[Not Supported]": no reading for this GPU, keep the others
		}

		temp, err := strconv.ParseFloat(field, 64)
		if err != nil {
			return nil, fmt.Errorf("parse GPU temperature in %q: %w", line, err)
		}
		gpus = append(gpus, GPU{Index: idx, Name: strings.TrimSpace(line[first+1 : last]), Celsius: temp})
	}

	return gpus, nil
}
