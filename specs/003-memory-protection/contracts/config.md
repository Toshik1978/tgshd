# Contract: `memory` configuration section

Amends [002 contracts/config.md](../../002-deeper-health-checks/contracts/config.md). A new
optional top-level section; absent ⇒ no memory check and no guard. Strict YAML as everywhere:
unknown keys are errors; an invalid section is one checker error and the section is skipped.

```yaml
memory:
  interval: 1m                       # common keys as for other checks
  threshold: 5m
  mode: tlg                          # warnings, and guard reports of a fixed problem
  warn_available_percent: 15         # 1-99
  warn_swap_percent: 80              # 1-100; ignored on hosts without swap
  warn_pressure_percent: 10          # some avg60, 1-100
  growth_window: 1h                  # 10m-24h
  growth_warn: 4GB                   # size: B, KB, MB, GB, TB (powers of 1024)
  audit_exclude: [ "scratch-job" ]   # containers allowed to run without a memory limit
  guard:
    intervene: report                # off | report | act   (default report)
    never_touch: [ "db", "backup.service" ]   # container names or systemd unit names
    danger_available_percent: 5      # 1-50, must be below warn_available_percent
    danger_pressure_percent: 20      # full avg10, 1-100
    danger_for: 30s                  # 5s-5m
    culprit_window: 10m              # 1m-1h
    culprit_min_growth: 1GB
    repeat_window: 6h                # 0 disables the stop rule (always restart)
    recovery_window: 2m              # 30s-30m
    action_timeout: 60s              # 10s-5m
    failed_mode: sms                 # tlg | sms | tlg-or-sms: reports when tgshd could not fix it
```

Validation: percentages in range; `danger_available_percent < warn_available_percent`;
durations in range; sizes positive; names in `never_touch` and `audit_exclude` match
`[A-Za-z0-9_.@-]+`. `pause_in_quiet_windows` applies to warnings only (default `true`); the guard
never pauses (spec edge cases). `needs_internet`, `depends_on` and `summary_group` are not
accepted.

## systemd unit (`config/tgshd.service`)

Adds, for the guard (research R7):

```ini
OOMScoreAdjust=-1000
LimitMEMLOCK=infinity
```
