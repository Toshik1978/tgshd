# Contract delta: Health configuration file

**Amends**: [001 contracts/config.md](../../001-host-health-monitoring/contracts/config.md).
Everything not named here is unchanged: the path (`$BOT_HEALTH_CONFIG`), strict YAML (unknown
keys are errors), per-entry validation, duration syntax, the run budget and timeouts.

**Compatibility**: every 001 file stays valid and loads without edits. New keys are optional.
Behaviour changes only where 002 deliberately changes a default or a judgement rule; those are
listed in "Changed defaults" and "Changed rules" below, each with the key that restores the 001
behaviour where one exists.

## New keys

| Section | Key | Type | Default | Validation | FR |
|---|---|---|---|---|---|
| every check (common) | `pause_in_quiet_windows` | bool | `false` for `disks`, `temperatures`, `kernel`; `true` for every other check | — | FR-026 |
| every check (common) | `summary_group` | string | none (one summary line per check) | `[a-z0-9-]+` | FR-046 |
| `general` | `maintenance_max_hold` | duration | `6h` | > 0 | FR-027 |
| `general` | `telegram_failing_after` | duration | `15m` | ≥ `1m` | FR-029 |
| `disks` | `spare_floor_percent` | int | `20` | 0–100 | FR-008 |
| `disks` | `force_read_after` | duration | `24h` | ≥ `1h` | FR-003 |
| `disks` | `include_serials` | [string] | `[]` | non-empty entries | FR-001 |
| `disks` | `extra_counters` | [int] | `[]` | ATA ids 1–255 | FR-006 |
| `disks` | `extra_counters_by_serial` | map serial → [int] | `{}` | ids 1–255 | FR-006 |
| `temperatures` | `limits_by_serial` | map serial → int (°C) | `{}` | 0–150 | FR-015 |
| `temperatures` | `board_sensors` | list of `{chip, label, limit}` | `[]` | `chip` required; `label` optional (all labels); `*` glob in both; `limit` 0–150, default `limits.board` | FR-017 |
| `http[]` | `follow_redirects` | bool | `false` | — | FR-037 |
| `http[]` | `auth_refusal` | `down` \| `checker_error` | `down` | one of | FR-039 |
| `http[].header`, `freshness[].header` | `placeholders` | [string] | `[]` | non-empty entries | FR-039 |
| `freshness[]` | `future_tolerance` | duration | `1h` | ≥ 0 | FR-042 |
| `freshness[]` | `url` | URL | none | http(s); exactly one of `path`, `url` | FR-043 |
| `freshness[]` | `timeout` | duration | `15s` | > 0, ≤ `20s`; only with `url` | FR-043 |
| `freshness[]` | `header` | `{name, value_env, prefix, placeholders}` | none | as `http[].header`; only with `url` | FR-043 |
| `command[]` | `expect_output` | regexp | none | compiles (Go RE2) | FR-041 |
| `command[]` | `forbid_output` | regexp | none | compiles (Go RE2) | FR-041 |
| `command[]` | `timeout_verdict` | `broken` \| `unreadable` | `broken` | one of | FR-041 |

## Changed defaults

| Key | 001 default | 002 default | Restore 001 with | FR |
|---|---|---|---|---|
| `disks.selftest_max_age` | `8d` (power-on time) | `40d` (calendar time since a passed test was newly observed) | closest: `selftest_max_age: 8d`; not an exact restore, because 002 counts calendar time where 001 counted power-on time | FR-011 |
| `temperatures.threshold` | `15m` | `0` | `threshold: 15m` | FR-014 |
| `temperatures.limits.cpu` | `85` | `90` (package sensor) | `cpu: 85` | FR-014 |
| `temperatures.limits.drive_nvme` | `70` | `60` | `drive_nvme: 70` | FR-014 |
| `temperatures.limits.drive_sata` | `55` | `60` (also SAS) | `drive_sata: 55` | FR-014 |
| `network.threshold` | `0` | the network `interval` (`5m`) | `threshold: 0` | FR-035 |
| `http[].follow_redirects` (implicit) | followed, up to 5 | not followed | `follow_redirects: true` | FR-037 |
| `pause_in_quiet_windows` (implicit) | every check paused | hardware checks run unless set to pause | `pause_in_quiet_windows: true` on `disks`, `temperatures`, `kernel` | FR-026 |

`temperatures.limits.gpu` (85) and `limits.board` (75) keep their values.

## Changed rules (no key; not configurable back)

| Rule | 001 | 002 | FR |
|---|---|---|---|
| Temperature comparison | PROBLEM above the limit | PROBLEM at or above the limit | FR-014 |
| Board sensors | every non-CPU sensor judged against `limits.board` | only `board_sensors` entries, or unlisted sensors against their own critical limit | FR-017 |
| CPU sensors | every coretemp/k10temp/zenpower input | one package sensor per CPU (`Package id N`; `Tdie`, else `Tctl`) | FR-016 |
| Drive discovery | every non-removable physical drive | NVMe, SATA, SAS only; USB and other transports need `include_serials`; a drive tracked by 001 that now classifies as USB or other and is not listed is retired silently with a log line, never reported missing | FR-001 |
| Drive error counters | ATA 5, 187, 188, 197, 198, 199; NVMe media errors and error-log entries | adds ATA 179, 181, 182, 183, 195; NVMe error-log entries no longer alert | FR-006, FR-007 |
| Network conditions | `network/router`, `network/internet` | one `network/connectivity`; stored 001 keys are cleared silently | FR-034 |
| Endpoint linked container missing | unreadable | down | FR-038 |
| DoH NXDOMAIN / NODATA | unreadable | down | FR-040 |
| Freshness stamp in the future | fresh | stale beyond `future_tolerance` | FR-042 |
| Checker-error rate limit | per check | per check, subject and kind | FR-028 |

## Example (002 keys only)

```yaml
general:
  maintenance_max_hold: 6h
  telegram_failing_after: 15m

disks:
  selftest_max_age: 40d                          # calendar days; 8d is the closest to 001, not an exact restore (001 counted power-on time)
  spare_floor_percent: 20
  force_read_after: 24h
  include_serials: [ "TEST0000000009" ]          # a USB drive the operator wants watched
  extra_counters: [ 252 ]
  extra_counters_by_serial:
    TEST0000000001: [ 230 ]
  # pause_in_quiet_windows: false                # default for hardware checks

temperatures:
  threshold: 0
  limits: { cpu: 90, drive_nvme: 60, drive_sata: 60, gpu: 85, board: 75 }
  limits_by_serial:
    TEST0000000002: 50
  board_sensors:
    - { chip: "nct*", label: "SYSTIN", limit: 70 }
    - { chip: "acpitz" }                         # all labels, limits.board

network:
  interval: 5m
  # threshold defaults to the interval

http:
  - name: web-app
    url: https://app.example.org/health
    follow_redirects: false
    auth_refusal: checker_error
    header: { name: Authorization, value_env: APP_TOKEN, prefix: "Bearer ", placeholders: [ "sample-key" ] }
    summary_group: web

freshness:
  - name: db-dump
    path: /var/lib/backup-status/db
    max_age: 26h
    future_tolerance: 1h
  - name: offsite-copy
    url: https://backup.example.org/last-success
    header: { name: X-Api-Key, value_env: BACKUP_STATUS_KEY }
    timeout: 15s
    max_age: 30h
    summary_group: backups

command:
  - name: port-guard
    run: [ "/usr/local/bin/probe-port" ]
    forbid_output: "(?i)degraded|error"
    timeout_verdict: unreadable
```

## Environment

No new environment variables ([001 environment contract](../../001-host-health-monitoring/contracts/environment.md)
unchanged). The secrets named by the new `freshness[].header.value_env` are ordinary environment
variables, as for `http`.
