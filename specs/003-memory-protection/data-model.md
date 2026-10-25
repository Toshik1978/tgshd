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
| `RestartPolicy` | containers only: Docker `HostConfig.RestartPolicy.Name`; the audit counts only `always` and `unless-stopped` |

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
`At` is within `repeat_window`; written by the guard through `Store.PutKV` (R11); `report` records are kept for the log but never count as a
restart for the repeat rule.

## Outcome (guard → reporter)

`{At, Kind, Service, Before, Growth, Window, AvailablePct, FullAvg10, TopProcess?, Err?}` with
`Kind ∈ {restarted, stopped, killed, would_restart, would_stop, not_recovered, protected,
no_culprit, action_failed}`. `restarted` and `would_*` use `mode`; the rest use `failed_mode`.

## Growth history (persisted, kv `memory.growth`)

`map[ServiceID][]{At, Anon}` — bounded per service; services gone for longer than
`growth_window` are dropped on export.

## Limit audit known set (persisted, kv `memory.audit`)

`{known []string, bad_for, last, open}` — the container names the audit counted on its last run
(running or restarting, restart policy `always` or `unless-stopped`, no memory limit, not in
`audit_exclude`), and the current streak's observed bad time, mirrored from the engine (a run adds
the time since the previous one, at most 1.5 intervals), with whether it has reached `threshold`
(the PROBLEM is open). Only while it is open is a counted container missing from `known`
announced, once. A container that leaves the set and rejoins while the problem stays open is a new
join and is announced again. Persisting it means a tgshd restart neither announces the open
problem's containers again nor treats a streak younger than `threshold` as open.

## Conditions and samples (existing engine and store)

| Key | Kind | Opens when |
|---|---|---|
| `memory/available` | state | `AvailablePct < warn_available_percent` for `threshold` |
| `memory/swap` | state | `SwapPct > warn_swap_percent` for `threshold` (hosts with swap) |
| `memory/pressure` | state | `SomeAvg60 > warn_pressure_percent` for `threshold` |
| `memory/growth/<service>` | state | research R12 |
| `memory/limit` | state | a running container with restart policy `always` or `unless-stopped`, without a limit, not in `audit_exclude`; one problem names them all, largest first |
| `memory/limit/<container>` | event | a container joins the counted set while the `memory/limit` PROBLEM is open |
| `memory/guard/<service>` | event | every guard outcome |
| `kernel/oom` | event | every out-of-memory kill (R13) |

Samples (existing `samples` table): `memory.available_pct`, `memory.swap_pct`,
`memory.pressure_some_avg60`, one per check run.

## Configuration

See [contracts/config.md](contracts/config.md).
