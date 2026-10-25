# Research: Memory Protection

Decisions behind [plan.md](plan.md). Each entry: decision, rationale, alternatives considered.

## R1. Overall shape: a tick check plus an independent guard (approach B)

- **Decision**: Two parts. A `memory` check in the existing 30 s health tick handles everything
  that is not urgent (warnings, growth trends, the limit audit, `/status`, `/history`, summary).
  A separate guard goroutine, started with the health runner but never scheduled by it, owns
  danger detection and intervention.
- **Rationale**: On the night that motivated the feature the tick's runs missed their deadline;
  an intervention queued behind other checks would have come too late (spec FR-008).
- **Alternatives**: (A) all inside the tick — rejected for that reason. (C) delegate to
  `systemd-oomd`/kernel — rejected: no growth-based victim choice, no restart-not-stop, no
  never-touch list; oomd kills and does not restart.

## R2. Signals and where they come from

- **Decision**: Host: `/proc/meminfo` (`MemTotal`, `MemAvailable`, `SwapTotal`, `SwapFree`) and
  `/proc/pressure/memory` (`some`/`full` `avg10`/`avg60`). Services: cgroup v2 under
  `/sys/fs/cgroup/system.slice` — `*.service` directories are systemd services,
  `docker-<id>.scope` directories are containers (systemd cgroup driver), and nested `*.slice`
  directories are walked one level down. Per service the guard reads `memory.stat` `anon` (plus
  `shmem`) and `memory.current`.
- **Rationale**: All are kernel files: cheap, no daemon involved, readable while the host
  thrashes. Growth is judged on anonymous memory, not `memory.current`, because page cache is
  reclaimable and a database's cache would otherwise look like a leak (SC-006).
- **Alternatives**: Docker stats API — slow, goes through a daemon that is itself starved;
  per-process RSS sums from `/proc` — expensive and racy; `memory.current` — counts cache.

## R3. Danger detection: polling, not PSI triggers

- **Decision**: The guard polls host signals every 1 s and per-service memory every 5 s.
  Danger = `MemAvailable/MemTotal < danger_available_percent` AND pressure `full avg10 ≥
  danger_pressure_percent`, both continuously for `danger_for`.
- **Rationale**: 1 s polling gives detection latency far below SC-001's 60 s and needs only
  file reads. `full` pressure means all non-idle tasks were stalled on memory — the thrashing
  state; `avg10` reacts within seconds. Requiring both signals avoids acting on a busy but
  healthy host (high pressure, plenty available) or a host that simply uses its memory (low
  available, no stalls).
- **Alternatives**: PSI triggers (`poll`/`epoll` on `/proc/pressure/memory` with a threshold
  written to it) — lower idle cost but more code (epoll loop, re-arming), and the gain over 1 s
  polling is irrelevant here; `golang.org/x/sys/unix` would be the natural helper and would need
  promotion from indirect (Principle III) — the stdlib `syscall` package would suffice, but the
  simpler polling design needs neither.

## R4. Culprit choice

- **Decision**: Over `culprit_window` (default 10 min) the guard keeps, per eligible service, the
  minimum `anon` seen; growth = current − minimum. The culprit is the service with the largest
  growth, provided it is at least `culprit_min_growth` (default 1 GiB). Ties go to the larger
  current size. No qualifying service ⇒ no culprit (R9).
- **Rationale**: Matches FR-003: the leaking service is the one growing; a big but stable service
  has near-zero growth. Minimum-in-window (not first sample) tolerates sawtooth usage.
- **Alternatives**: largest size (kills databases — rejected by the operator); rate over the last
  minute only (noisy; a startup burst would win).

## R5. Eligibility

- **Decision**: Eligible = direct and nested children of `system.slice` that are `*.service` or
  `docker-*.scope`, minus: `never_touch` names (container names or unit names), tgshd's own unit
  (read from `/proc/self/cgroup`), and a built-in core list (`docker.service`,
  `containerd.service`, `dbus.service`, `ssh.service`, `sshd.service`, `systemd-*`). Everything
  under `user.slice` and `init.scope` is never eligible; processes there count as stray (R9).
- **Rationale**: FR-004 — unknown culprit, operator-protected services, never the host's core.
- **Alternatives**: opt-in list of killable services — rejected by the operator (tomorrow's
  culprit is unknown).

## R6. Acting: restart, stop, and the runtime-unresponsive fallback

- **Decision**: Containers: Docker Engine API `ContainerRestart` / `ContainerStop` through the
  existing `pkg/host/docker` client (adds `Stop`). systemd services: exec `systemctl restart|stop
  <unit>` (same pattern as `journalctl`, R7 of 002). Each action is bounded by `action_timeout`
  (default 60 s). If the action times out or the runtime cannot be reached, the guard writes `1`
  to the service's `cgroup.kill` (cgroup v2, Linux ≥ 5.14): the kernel kills every process of
  that service at once, freeing its memory; Docker's or systemd's restart policy then decides
  whether it comes back. That outcome is always reported by SMS.
- **Rationale**: Under heavy thrashing `dockerd` itself is likely starved; an API call that never
  answers would leave the leak running. `cgroup.kill` needs no daemon and is the one action that
  is certain to free memory. This refines the spec's edge case "runtime unreachable: no action
  possible" into "fallback kill, reported by SMS" (spec amended accordingly).
- **Alternatives**: D-Bus to systemd — needs a new dependency (`godbus`); `cgroup.kill` as the
  primary action — skips graceful shutdown and the restart semantics the operator wants.

## R7. Shielding the guard from starvation

- **Decision**: (1) The tgshd unit gets `OOMScoreAdjust=-1000` and `LimitMEMLOCK=infinity`, and
  the daemon also writes `-1000` to `/proc/self/oom_score_adj` when the guard starts.
  (2) The guard calls `mlockall(MCL_CURRENT|MCL_FUTURE|MCL_ONFAULT)` through `syscall.Syscall`
  (`MCL_ONFAULT` = 4, defined locally; Linux ≥ 4.4), so tgshd's pages — binary text included —
  are never swapped out, while Go's large address-space reservations are locked only as they are
  touched. (3) The decision path uses preallocated per-service rings, reads only kernel files, and
  never touches the store, the network or Telegram; reports and persistence go through buffered
  channels to other goroutines (R10). A failed `mlockall` is a checker error, not fatal.
- **Rationale**: FR-008. On the motivating night tgshd itself stalled on page faults; locked
  memory and OOM protection remove both failure modes for a process whose RSS is tens of MB.
- **Alternatives**: `MCL_FUTURE` without `MCL_ONFAULT` — would populate Go's 64 MB heap arenas
  eagerly; a separate guard process — more deployment surface for no gain.

## R8. Episode state machine

- **Decision**: `idle → danger(since) → acted(at, service, action) → (cleared | not-recovered)`.
  `danger` becomes active after `danger_for`. After an action the guard waits `recovery_window`
  (default 2 min); danger cleared ⇒ episode ends; still in danger ⇒ one "did not free memory"
  report and the episode stays `not-recovered` (no second action) until danger has been clear
  for `recovery_window`. An action whose service was acted on within `repeat_window` (default
  6 h) is `stop` instead of `restart`. In `report` mode the same machine runs and every action
  becomes a "would …" report.
- **Rationale**: FR-005, FR-006; one machine for both modes keeps `report` an honest preview.

## R9. No culprit and stray processes

- **Decision**: Danger with no qualifying culprit ⇒ the guard scans `/proc/*/status` (`VmRSS`,
  `Name`, cgroup) once per episode and reports the top process and which cgroup it is in, by SMS.
  A culprit on the never-touch list ⇒ "did not act, protected" by SMS. Both once per episode.
- **Rationale**: FR-011 and acceptance scenario 5.

## R10. Reporting path

- **Decision**: The guard emits outcome records on a buffered channel; a reporter goroutine turns
  them into `notify.Message` events (key `memory/guard/<service>`, mode `mode` for fixed and
  `failed_mode` for not fixed) through a new `Runner.PushMode`, and into alert-log records. A full
  channel drops the oldest record and logs it; the decision path never blocks.
- **Rationale**: Messages queue and retry through the existing notifier when Telegram or the
  network is down; delivery may lag the action (FR-008 allows it).

## R11. Persistence

- **Decision**: Intervention records (service, action, at, before) are kept in the guard and
  written by the guard itself as kv `memory.guard` (JSON) through `Store.PutKV`: as soon as an
  action completes (a writer goroutine off the decision path, retried every tick on failure) and
  once more on stop. The runner's tick checkpoint only restores the key at start and writes
  nothing for it. Records that load late (the store was unreadable at start) are handed to the
  running guard, which merges them without blocking its decision path. Records older than
  `repeat_window` are dropped. The tick check's per-service growth history is a
  bounded ring (one point per check run, `growth_window` deep) exported as kv `memory.growth`.
  Host-level metrics (`memory.available_pct`, `memory.swap_pct`, `memory.pressure_some_avg60`)
  go to the existing `samples` table.
- **Rationale**: FR-009, FR-017. FR-009 needs the record in the store before a restart that
  follows an action closely, so it cannot wait for the next checkpoint. The checkpoint replaces
  the stored state as a whole, so one that carried only this kv would erase the stored
  conditions and queue; `PutKV` upserts the one key and touches nothing else, and the guard owns
  that key. Per-service raw samples in `samples` (90-day retention) would
  reach millions of rows; the growth ring only needs the window.

## R12. Early-warning growth judgement

- **Decision**: For each service, over `growth_window` (default 1 h): rise = latest − minimum;
  PROBLEM when rise ≥ `growth_warn` (default 4 GiB) AND the minimum precedes the maximum AND the
  latest point is ≥ 90 % of the window's maximum (still at its peak). Resolves when the rise falls
  below half of `growth_warn` or the service disappears.
- **Rationale**: A leak climbs and stays up; a flushed cache falls back (latest far below max), so
  it never opens (acceptance scenario US2-5, SC-006). Half-threshold hysteresis avoids flapping.

## R13. Out-of-memory kills in the kernel log

- **Decision**: A new kernel class `kernel/oom` with its own formatter. It pairs the
  `oom-kill:constraint=…,oom_memcg=…,task_memcg=…,task=…,pid=…` record with the following
  `Out of memory: Killed process <pid> (<name>) … anon-rss:<kB>` or `Memory cgroup out of memory:
  Killed process …` record, and emits one event per kill: process, pid, anon RSS, the service
  resolved from `task_memcg` (container name through the Docker name map; unit name otherwise),
  and `constraint` (`CONSTRAINT_MEMCG` ⇒ "hit its own limit"). At most 5 events per run, then
  "and N more". The previous boot's journal tail is read as in 002.
- **Rationale**: FR-019 and SC-007; the existing per-class "found (n records)" summary hides
  which service died.

## R14. Defaults

| Setting | Default | Why |
|---|---|---|
| `interval` / `threshold` | 1m / 5m | warnings are not urgent; the guard handles urgency |
| `warn_available_percent` | 15 | about ten minutes of headroom at 1 GB/min on 64 GB |
| `warn_swap_percent` | 80 | |
| `warn_pressure_percent` | 10 (`some avg60`) | sustained stalls without thrashing |
| `growth_window` / `growth_warn` | 1h / 4GB | SC-003: 20 GB/h warns ≥ 30 min before danger |
| `guard.intervene` | `report` | clarification 2 |
| `guard.danger_available_percent` | 5 | |
| `guard.danger_pressure_percent` | 20 (`full avg10`) | |
| `guard.danger_for` | 30s | SC-001: act within 60 s |
| `guard.culprit_window` / `culprit_min_growth` | 10m / 1GB | |
| `guard.repeat_window` | 6h | |
| `guard.recovery_window` | 2m | |
| `guard.action_timeout` | 60s | |
| `mode` / `guard.failed_mode` | `tlg` / `sms` | clarification on channels |
