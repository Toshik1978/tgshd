package nvidia_test

import (
	"context"
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
	want := []nvidia.GPU{
		{Index: 0, Name: "Example GPU", Celsius: 54},
		{Index: 1, Name: "Example GPU, Pro", Celsius: 61},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %+v", got)
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
	if err != nil || len(got) != 1 || got[0].Celsius != 70 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestGPUsFailingBinary(t *testing.T) {
	bin := writeBin(t, "#!/bin/sh\nexit 9\n")
	if _, err := nvidia.New(bin).GPUs(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseSkipsUnavailable(t *testing.T) {
	got, err := nvidia.Parse("0, Example GPU, [N/A]\n1, Example GPU, [Not Supported]\n2, Example GPU, 48\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Index != 2 || got[0].Celsius != 48 {
		t.Fatalf("got %+v", got)
	}
}
