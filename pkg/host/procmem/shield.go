//go:build linux

package procmem

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

const (
	oomScoreAdjPath = "/proc/self/oom_score_adj"
	oomScoreAdjMin  = "-1000"
	// mclOnFault is MCL_ONFAULT (Linux >= 4.4): lock pages only as they are
	// touched. The syscall package does not define it.
	mclOnFault = 4
)

// Shield protects the daemon from the OOM killer and from being paged out, so
// the memory guard keeps running while the host starves.
type Shield struct {
	oomPath  string
	mlockall func(flags uintptr) error
}

// NewShield creates a Shield over the real /proc and the mlockall syscall.
func NewShield() *Shield {
	return &Shield{oomPath: oomScoreAdjPath, mlockall: syscallMlockall}
}

// Apply writes -1000 to oom_score_adj and locks the process memory. Both steps
// are always attempted; failures are joined and left to the caller to raise.
func (s *Shield) Apply() error {
	var errs []error
	if err := os.WriteFile(s.oomPath, []byte(oomScoreAdjMin), 0o600); err != nil {
		errs = append(errs, fmt.Errorf("set oom_score_adj: %w", err))
	}
	flags := uintptr(syscall.MCL_CURRENT | syscall.MCL_FUTURE | mclOnFault)
	if err := s.mlockall(flags); err != nil {
		errs = append(errs, fmt.Errorf("mlockall: %w", err))
	}

	return errors.Join(errs...)
}

func syscallMlockall(flags uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_MLOCKALL, flags, 0, 0); errno != 0 {
		return fmt.Errorf("syscall: %w", errno)
	}
	return nil
}
