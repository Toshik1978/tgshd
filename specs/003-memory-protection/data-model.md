# Data Model: Memory Protection

All new types live in pure packages (`pkg/health/check/memory`, `pkg/health/memguard`) unless
marked as adapter output. Sizes are bytes (`uint64`), percentages `float64` 0–100.

## Host memory snapshot (adapter output → pure input)

| Field | Source | Notes |
|---|---|---|
| `At` | clock | |
| `Total`, `Available` | `/proc/meminfo` `MemTotal`, `MemAvailable` | |
| `SwapTotal`, `SwapFree` | `/proc/meminfo` | `SwapTotal == 0` ⇒ no swap judgement |
| `SomeAvg10`, `SomeAvg60`, `FullAvg10`, `FullAvg60` | `/proc/pressure/memory` | missing file (PSI disabled) ⇒ pressure judgements skipped, one checker error |

Derived: `AvailablePct = Available/Total·100`, `SwapPct = (SwapTotal−SwapFree)/SwapTotal·100`.

## Service

| Field | Notes |
|---|---|
| `ID` | stable key: `container:<name>` or `unit:<name.service>` |
| `Kind` | `container` \| `unit` |
| `Name` | container name (from the Docker name map; short id when unknown) or unit name |
| `CgroupPath` | under `/sys/fs/cgroup`, used for reads and `cgroup.kill` |
| `Eligible` | false for never-touch, core, tgshd's own unit (research R5) |
| `Protected` | true when excluded by `never_touch` (reported as "protected", not silently skipped) |
| `HasLimit` | containers only: Docker `HostConfig.Memory > 0`; audit input |

## Service memory sample

`{At, ServiceID, Anon, Current}` — `Anon` = `memory.stat` `anon + shmem`; `Current` =
`memory.current`. The guard keeps a preallocated ring per service (`culprit_window / 5 s` deep);
the check keeps one point per run (`growth_window / interval` deep).

## Danger episode (guard, in memory only)

```text
idle ──(danger held for danger_for)──▶ danger
danger ──(culprit found, intervene=act)──▶ acted
danger ──(no culprit | protected | intervene=report)──▶ reported   (one report; no action)
acted ──(danger clear within recovery_window)──▶ idle   (report: fixed)
acted ──(still danger after recovery_window)──▶ not_recovered   (one SMS; no second action)
reported | not_recovered ──(danger clear for recovery_window)──▶ idle
```

Fields: `Since`, `State`, `Culprit` (service, before-size, growth, window), `Action`, `ActedAt`.

## Intervention record (persisted, kv `memory.guard`)

`{ServiceID, Name, Action (restart|stop|kill), At, Before, Mode (act|report)}` — kept while
`At` is within `repeat_window`; `report` records are kept for the log but never count as a
restart for the repeat rule.

## Outcome (guard → reporter)

`{At, Kind, Service, Before, Growth, Window, AvailablePct, FullAvg10, TopProcess?, Err?}` with
`Kind ∈ {restarted, stopped, killed, would_restart, would_stop, not_recovered, protected,
no_culprit, action_failed}`. `restarted` and `would_*` use `mode`; the rest use `failed_mode`.

## Growth history (persisted, kv `memory.growth`)

`map[ServiceID][]{At, Anon}` — bounded per service; services gone for longer than
`growth_window` are dropped on export.

## Limit audit known set (persisted, kv `memory.audit`)

`[]string` — the container names the audit last judged, so a `memory/limit/<container>`
condition for a container removed while the daemon was down resolves after the restart.

## Conditions and samples (existing engine and store)

| Key | Kind | Opens when |
|---|---|---|
| `memory/available` | state | `AvailablePct < warn_available_percent` for `threshold` |
| `memory/swap` | state | `SwapPct > warn_swap_percent` for `threshold` (hosts with swap) |
| `memory/pressure` | state | `SomeAvg60 > warn_pressure_percent` for `threshold` |
| `memory/growth/<service>` | state | research R12 |
| `memory/limit/<container>` | state | running container without a limit, not in `audit_exclude` |
| `memory/guard/<service>` | event | every guard outcome |
| `kernel/oom` | event | every out-of-memory kill (R13) |

Samples (existing `samples` table): `memory.available_pct`, `memory.swap_pct`,
`memory.pressure_some_avg60`, one per check run.

## Configuration

See [contracts/config.md](contracts/config.md).
