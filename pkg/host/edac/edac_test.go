package edac_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Toshik1978/tgshd/pkg/host/edac"
)

func writeMC(t *testing.T, root, mc, ce, ue string) {
	t.Helper()
	d := filepath.Join(root, mc)
	if err := os.MkdirAll(d, 0o750); err != nil {
		t.Fatal(err)
	}
	if ce != "" {
		if err := os.WriteFile(filepath.Join(d, "ce_count"), []byte(ce), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if ue != "" {
		if err := os.WriteFile(filepath.Join(d, "ue_count"), []byte(ue), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadAbsent(t *testing.T) {
	got, err := edac.New(filepath.Join(t.TempDir(), "mc")).Read()
	if err != nil || got != nil {
		t.Fatalf("got %v, %v; want nil, nil", got, err)
	}
}

func TestReadEmptyDir(t *testing.T) {
	got, err := edac.New(t.TempDir()).Read()
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestRead(t *testing.T) {
	root := t.TempDir()
	writeMC(t, root, "mc1", "0\n", "2\n")
	writeMC(t, root, "mc0", "5\n", "0\n")
	if err := os.MkdirAll(filepath.Join(root, "power"), 0o750); err != nil {
		t.Fatal(err)
	}
	got, err := edac.New(root).Read()
	if err != nil {
		t.Fatal(err)
	}
	want := []edac.Counters{{Controller: "mc0", Correctable: 5}, {Controller: "mc1", Uncorrectable: 2}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestPerControllerErr(t *testing.T) {
	root := t.TempDir()
	writeMC(t, root, "mc0", "x\n", "0\n")
	writeMC(t, root, "mc1", "1\n", "")
	writeMC(t, root, "mc2", "4\n", "6\n")

	got, err := edac.New(root).Read()
	if err != nil {
		t.Fatalf("Read() error = %v; want per-controller errors only", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d controllers, want 3: %+v", len(got), got)
	}
	for _, i := range []int{0, 1} {
		if got[i].Err == nil {
			t.Errorf("%s: Err = nil, want an error", got[i].Controller)
		}
	}
	if got[2].Err != nil || got[2].Correctable != 4 || got[2].Uncorrectable != 6 {
		t.Errorf("mc2 = %+v, want 4/6 with no error", got[2])
	}
}
