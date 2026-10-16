package config

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
)

// problems accumulates validation failures of one entry.
type problems []string

func (p *problems) addf(format string, args ...any) {
	*p = append(*p, fmt.Sprintf(format, args...))
}

func (p *problems) reason() string {
	return strings.Join(*p, "; ")
}

func (p *problems) name(n string) {
	if !validName(n) {
		p.addf("name %q must match [a-z0-9-]+", n)
	}
}

func (p *problems) interval(key string, d Duration) {
	if d.Duration() < MinInterval {
		p.addf("%s %v must be at least %v", key, d.Duration(), MinInterval)
	}
}

func (p *problems) nonNegative(key string, d Duration) {
	if d < 0 {
		p.addf("%s %v must not be negative", key, d.Duration())
	}
}

func (p *problems) positive(key string, d Duration) {
	if d <= 0 {
		p.addf("%s %v must be greater than zero", key, d.Duration())
	}
}

// checkTimeout validates a per-check timeout: positive and at most
// health.MaxCheckTimeout, so it ends well inside the runner's run budget and
// a hung probe is judged Bad instead of abandoned.
func (p *problems) checkTimeout(key string, d Duration) {
	p.positive(key, d)

	if d.Duration() > health.MaxCheckTimeout {
		p.addf("%s %v must be at most %v", key, d.Duration(), health.MaxCheckTimeout)
	}
}

func (p *problems) atLeast(key string, v, lowest int) {
	if v < lowest {
		p.addf("%s %d must be at least %d", key, v, lowest)
	}
}

func (p *problems) required(key, v string) {
	if v == "" {
		p.addf("%s is required", key)
	}
}

func (p *problems) oneOf(key, v string, allowed ...string) {
	if !slices.Contains(allowed, v) {
		p.addf("%s %q must be one of %s", key, v, strings.Join(allowed, ", "))
	}
}

func (p *problems) common(c *Common) {
	p.interval("interval", c.Interval)
	p.nonNegative("threshold", c.Threshold)
	p.oneOf("mode", string(c.Mode), string(ModeTLG), string(ModeSMS), string(ModeTLGOrSMS))
}

func (p *problems) schedule(weekdayName, clock string) (time.Weekday, bool) {
	wd, ok := parseWeekday(weekdayName)
	if !ok {
		p.addf("weekday %q is not a day of the week", weekdayName)
	}

	if _, err := time.Parse("15:04", clock); err != nil {
		p.addf("time %q must be HH:MM", clock)
	}

	return wd, ok
}

func validName(n string) bool {
	if n == "" {
		return false
	}

	for _, c := range n {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}

	return true
}

func parseWeekday(s string) (time.Weekday, bool) {
	for d := time.Sunday; d <= time.Saturday; d++ {
		if strings.EqualFold(s, d.String()) {
			return d, true
		}
	}

	return time.Sunday, false
}

func sortedStrings(s []string) []string {
	slices.Sort(s)

	return s
}

func validateGeneral(g *General, p *problems) {
	p.required("state_dir", g.StateDir)
	p.required("heartbeat_file", g.HeartbeatFile)
	p.nonNegative("boot_grace", g.BootGrace)
	p.atLeast("concurrency", g.Concurrency, 1)
}

func validateSummary(s *Summary, p *problems) {
	s.Weekday, _ = p.schedule(s.WeekdayName, s.Time)
}

func validateNetwork(n *Network, p *problems) {
	p.common(&n.Common)

	if len(n.InternetTargets) == 0 {
		p.addf("internet_targets needs at least one address")
	}

	if slices.Contains(n.InternetTargets, "") {
		p.addf("internet_targets must not contain empty entries")
	}
}

func validateUPS(u *UPS, p *problems) {
	p.common(&u.Common)
}

func validateDisks(d *Disks, p *problems) {
	p.common(&d.Common)

	if d.WearPercent < 0 || d.WearPercent > 100 {
		p.addf("wear_percent %d must be within 0-100", d.WearPercent)
	}

	p.positive("selftest_max_age", d.SelftestMaxAge)

	if d.SpareFloorPercent < 0 || d.SpareFloorPercent > 100 {
		p.addf("spare_floor_percent %d must be within 0-100", d.SpareFloorPercent)
	}

	if d.ForceReadAfter.Duration() < time.Hour {
		p.addf("force_read_after %v must be at least 1h", d.ForceReadAfter.Duration())
	}

	if slices.Contains(d.IncludeSerials, "") {
		p.addf("include_serials must not contain empty entries")
	}

	validateCounterIDs(p, "extra_counters", d.ExtraCounters)

	for _, serial := range slices.Sorted(maps.Keys(d.ExtraCountersBySerial)) {
		if serial == "" {
			p.addf("extra_counters_by_serial must not have an empty serial key")
		}

		validateCounterIDs(p, "extra_counters_by_serial "+serial, d.ExtraCountersBySerial[serial])
	}
}

// validateCounterIDs requires every entry to be an ATA attribute id within 1-255.
func validateCounterIDs(p *problems, key string, ids []int) {
	for _, id := range ids {
		if id < 1 || id > 255 {
			p.addf("%s id %d must be within 1-255", key, id)
		}
	}
}

func validateTemperatures(t *Temperatures, p *problems) {
	p.common(&t.Common)

	for _, l := range []struct {
		key string
		v   int
	}{
		{"cpu", t.Limits.CPU},
		{"drive_nvme", t.Limits.DriveNVMe},
		{"drive_sata", t.Limits.DriveSATA},
		{"gpu", t.Limits.GPU},
		{"board", t.Limits.Board},
	} {
		p.atLeast("limits."+l.key, l.v, 0)
	}
}

func validateKernel(k *Kernel, p *problems) {
	p.common(&k.Common)
}

func validateContainers(c *Containers, p *problems) {
	p.common(&c.Common)
}

func validateModemProbe(m *ModemProbe, p *problems) {
	p.required("phone_env", m.PhoneEnv)
	m.Weekday, _ = p.schedule(m.WeekdayName, m.Time)
	p.positive("reply_within", m.ReplyWithin)
	p.positive("max_silence", m.MaxSilence)
}

func validateHTTP(h *HTTP, p *problems) {
	p.common(&h.Common)
	p.checkTimeout("timeout", h.Timeout)
	validateURL(p, "url", h.URL)

	if lo, hi := h.Status[0], h.Status[1]; lo < 100 || hi > 599 || lo > hi {
		p.addf("status %v must be an ascending range within 100-599", h.Status)
	}

	validateHTTPRouting(h, p)

	if h.Header != nil {
		p.required("header.name", h.Header.Name)
		p.required("header.value_env", h.Header.ValueEnv)
	}

	if h.DependsOn != "" {
		kind, name, ok := strings.Cut(h.DependsOn, "/")
		if !ok || !slices.Contains(checkKinds(), kind) || !validName(name) {
			p.addf("depends_on %q must look like kind/name with kind one of %s",
				h.DependsOn, strings.Join(checkKinds(), ", "))
		}
	}
}

func validateHTTPRouting(h *HTTP, p *problems) {
	if h.ConnectTo != "" && h.DoH != "" {
		p.addf("connect_to and doh are mutually exclusive")
	}

	if h.DoH != "" {
		validateURL(p, "doh", h.DoH)

		h.NeedsInternet = true
	}
}

func checkKinds() []string {
	return []string{
		"network", "ups", "disks", "temperatures", "kernel", "containers",
		"http", "freshness", "command", "modem_probe",
	}
}

func validateURL(p *problems, key, raw string) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		// The value is not repeated: its userinfo or query may be secret, and
		// entry errors are pushed to Telegram and email.
		p.addf("%s must be an http or https URL", key)
	}
}

func validateFreshness(f *Freshness, p *problems) {
	p.common(&f.Common)
	p.required("path", f.Path)
	p.oneOf("content", string(f.Content), string(ContentEpoch), string(ContentMtime))
	p.positive("max_age", f.MaxAge)
}

func validateCommand(c *Command, p *problems) {
	p.common(&c.Common)
	p.checkTimeout("timeout", c.Timeout)
	p.oneOf("report", string(c.Report), string(ReportJournal), string(ReportAlert))

	if len(c.Run) == 0 || c.Run[0] == "" {
		p.addf("run needs a non-empty argv")
	}

	if c.Fix != nil {
		validateFix(c.Fix, p)
	}
}

func validateFix(f *Fix, p *problems) {
	d := defaultFix()
	f.After = cmpDefault(f.After, d.After)
	f.MaxPerHour = cmpDefault(f.MaxPerHour, d.MaxPerHour)

	if f.GraceAfterStart == 0 {
		f.GraceAfterStart = d.GraceAfterStart
	}

	p.atLeast("fix.after", f.After, 1)
	p.atLeast("fix.max_per_hour", f.MaxPerHour, 1)
	p.nonNegative("fix.grace_after_start", f.GraceAfterStart)

	if len(f.Run) > 0 && f.Run[0] == "" {
		p.addf("fix.run needs a non-empty argv[0]")
	}

	if (f.RestartContainer == "") == (len(f.Run) == 0) {
		p.addf("fix needs exactly one of restart_container or run")
	}
}

func cmpDefault(v, def int) int {
	if v == 0 {
		return def
	}

	return v
}
