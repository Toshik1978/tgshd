package nvidia_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Toshik1978/tgshd/pkg/host/nvidia"
)

func writeBin(t *testing.T, script string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "nvidia-smi")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	return bin
}

func TestParse(t *testing.T) {
	in := "0, Example GPU, 54\n1, Example GPU, Pro, 61\n\n"
	got, err := nvidia.Parse(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Index != 0 || got[0].Name != "Example GPU" || got[0].Celsius == nil || *got[0].Celsius != 54 {
		t.Fatalf("got[0] = %+v", got[0])
	}
	if got[1].Index != 1 || got[1].Name != "Example GPU, Pro" || got[1].Celsius == nil || *got[1].Celsius != 61 {
		t.Fatalf("got[1] = %+v", got[1])
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{"x, Example GPU, 50", "0, Example GPU, hot", "0, 50"} {
		if _, err := nvidia.Parse(in); err == nil {
			t.Errorf("Parse(%q): expected error", in)
		}
	}
}

func TestGPUsAbsentBinary(t *testing.T) {
	got, err := nvidia.New(filepath.Join(t.TempDir(), "nvidia-smi")).GPUs(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestGPUsRunsBinary(t *testing.T) {
	bin := writeBin(
		t,
		"#!/bin/sh\n[ \"$1\" = --query-gpu=index,name,temperature.gpu ] && [ \"$2\" = --format=csv,noheader,nounits ] || exit 2\necho '0, Example GPU, 70'\n",
	)
	got, err := nvidia.New(bin).GPUs(context.Background())
	if err != nil || len(got) != 1 || got[0].Celsius == nil || *got[0].Celsius != 70 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestGPUsFailingBinary(t *testing.T) {
	bin := writeBin(t, "#!/bin/sh\nexit 9\n")
	if _, err := nvidia.New(bin).GPUs(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestNoDevicesSentinel(t *testing.T) {
	for name, script := range map[string]string{
		"stdout":    "#!/bin/sh\necho 'No devices were found'\nexit 6\n",
		"stderr":    "#!/bin/sh\necho 'No devices were found' >&2\nexit 6\n",
		"mixedcase": "#!/bin/sh\necho 'NO DEVICES WERE FOUND.' >&2\nexit 6\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := nvidia.New(writeBin(t, script)).GPUs(context.Background())
			if !errors.Is(err, nvidia.ErrNoDevices) || got != nil {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
}

func TestGPUsOtherFailuresStayErrors(t *testing.T) {
	for name, script := range map[string]string{
		"driver mismatch": "#!/bin/sh\necho 'Failed to initialize NVML: Driver/library version mismatch' >&2\nexit 18\n",
		"zero exit":       "#!/bin/sh\necho 'No devices were found'\nexit 0\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := nvidia.New(writeBin(t, script)).GPUs(context.Background()); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestNAKept(t *testing.T) {
	got, err := nvidia.Parse("0, Example GPU, [N/A]\n1, Example GPU, [Not Supported]\n2, Example GPU, 48\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Index != 0 || got[0].Celsius != nil || got[1].Index != 1 || got[1].Celsius != nil {
		t.Fatalf("unavailable GPUs = %+v, %+v", got[0], got[1])
	}
	if got[2].Index != 2 || got[2].Celsius == nil || *got[2].Celsius != 48 {
		t.Fatalf("got[2] = %+v", got[2])
	}
}
