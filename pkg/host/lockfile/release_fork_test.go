package lockfile

import (
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// TestReleaseFreesTheLockWhileChildrenFork: a child forked while the lock is
// held shares its open file until it execs, so a lock dropped only by closing
// the file can outlive Release. Release must free it at once.
func TestReleaseFreesTheLockWhileChildrenFork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.lock")

	var stop atomic.Bool
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for !stop.Load() {
				_ = exec.CommandContext(t.Context(), "/bin/true").Run()
			}
		})
	}
	defer wg.Wait()
	defer stop.Store(true)

	for i := range 2000 {
		inst, err := AcquireInstance(path)
		if err != nil {
			t.Fatalf("acquire %d right after a release: %v", i, err)
		}
		if err := inst.Release(); err != nil {
			t.Fatal(err)
		}
	}
}
