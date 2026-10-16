package report

import (
	"fmt"
	"strings"

	"github.com/Toshik1978/tgshd/pkg/health/runner"
)

// Retired renders the reply to a successful /retire.
func Retired(d runner.Drive, cleared int) string {
	return Truncate(fmt.Sprintf("Retired %s; cleared %d conditions.", escape(d.Label()), cleared))
}

// Drives renders a heading followed by one drive per line.
func Drives(heading string, drives []runner.Drive) string {
	var b strings.Builder
	b.WriteString(escape(heading))
	if len(drives) == 0 {
		b.WriteString("\nNo known drives")
	}
	for _, d := range drives {
		b.WriteString("\n" + escape(d.Label()))
	}

	return Truncate(b.String())
}
