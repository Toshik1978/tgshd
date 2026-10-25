---

description: "Task list for Memory Protection"
---

# Tasks: Memory Protection

**Input**: Design documents from `specs/003-memory-protection/`

**Prerequisites**: [plan.md](plan.md), [spec.md](spec.md), [research.md](research.md), [data-model.md](data-model.md), [contracts/](contracts/), [quickstart.md](quickstart.md)

**Tests**: Every task is implemented test-first (constitution II): each task writes its own tests,
named in the task, before the code. There are no standalone test tasks.

**Organization**: Tasks are grouped by user story so each story can be implemented and tested
independently. No new dependency may be added (plan Technical Context).

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies on incomplete tasks)
- **[Story]**: Which user story the task belongs to (US1–US4)

---

## Phase 1: Foundational (Blocking Prerequisites)

**Purpose**: Configuration, kernel-file adapters and delivery hooks that every story uses.

**⚠️ CRITICAL**: No user story work can begin until this phase is complete.

- [x] T001 [P] Add the `memory` section with the `Size` type (B, KB, MB, GB, TB as powers of 1024) to pkg/health/config/types.go, pkg/health/config/checks.go, pkg/health/config/defaults.go and pkg/health/config/validate.go exactly as in contracts/config.md: defaults from research R14 (`intervene` defaults to `report` when the section is present); ranges "warn_available_percent 1-99", "warn_swap_percent 1-100", "warn_pressure_percent 1-100", "growth_window 10m-24h", "danger_available_percent 1-50, must be below warn_available_percent", "danger_pressure_percent 1-100", "danger_for 5s-5m", "culprit_window 1m-1h", "repeat_window 0 disables the stop rule", "recovery_window 30s-30m", "action_timeout 10s-5m"; names in `never_touch` and `audit_exclude` match `[A-Za-z0-9_.@-]+`; `needs_internet`, `depends_on`, `summary_group` rejected; `pause_in_quiet_windows` default true (tests in pkg/health/config/config_test.go: defaults, every range edge, the danger/warn cross-check, Size parsing) (FR-001, FR-020)
- [x] T002 [P] Host memory adapter in pkg/host/procmem/procmem.go: parse `/proc/meminfo` (`MemTotal`, `MemAvailable`, `SwapTotal`, `SwapFree`) and `/proc/pressure/memory` (`some`/`full` `avg10`/`avg60`) from an injectable root, a missing PSI file returned as a sentinel error; top processes from `/proc/*/status` (`Name`, `VmRSS`) with their cgroup from `/proc/<pid>/cgroup` (tests in pkg/host/procmem/procmem_test.go against synthetic trees in `t.TempDir()`) (FR-011, FR-012, R2, R9)
- [x] T003 [P] cgroup adapter in pkg/host/cgroupmem/cgroupmem.go: walk `system.slice` and nested `*.slice` one level down, return `*.service` and `docker-<id>.scope` entries with `memory.stat` `anon`+`shmem` and `memory.current`; `Kill(path)` writes `1` to `cgroup.kill`; `Self()` reads tgshd's own cgroup from `/proc/self/cgroup`; injectable root (tests in pkg/host/cgroupmem/cgroupmem_test.go against synthetic trees) (FR-003, R2, R5, R6)
- [x] T004 [P] Docker adapter: add `Stop`, the container ID and `HostConfig.Memory` (limit) to `Container` in pkg/host/docker/docker.go, extending the `engine` interface with `ContainerStop` (tests in pkg/host/docker/docker_test.go with the existing fake engine) (FR-005, FR-018, R6)
- [x] T005 [P] systemd adapter in pkg/host/systemctl/systemctl.go: `Restart(ctx, unit)` and `Stop(ctx, unit)` exec `systemctl restart|stop <unit>` bounded by the context; the unit name validated against `[A-Za-z0-9_.@-]+\.service` before exec (tests in pkg/host/systemctl/systemctl_test.go with an injected command runner) (FR-003, FR-010, R6)
- [x] T006 Add `Runner.PushMode(ctx, check, key, text, record string, mode health.Mode)` in pkg/health/runner/runner.go: an event with a delivery mode and a key distinct from the check, logged like `Push` (tests in pkg/health/runner/runner_test.go: `TestPushModeDeliversBySMS`, `TestPushModeQueuesWhenTelegramFails`) (FR-007, R10)

**Checkpoint**: Foundation ready — user stories can begin.

---

## Phase 2: User Story 1 - A runaway service is restarted before the host starves (Priority: P1) 🎯 MVP

**Goal**: The guard detects danger independently of the tick, restarts (or stops) the service that grew the most, and reports what it did.

**Independent Test**: quickstart.md "Guard" table — fake clock, sampler, actuator and reporter; plus the manual host run.

- [x] T007 [P] [US1] Pure guard model in pkg/health/memguard/guard.go and pkg/health/memguard/culprit.go: `Config`, `Service` (`ID` = `container:<name>` | `unit:<name.service>`, `Kind`, `Name`, `CgroupPath`, `Eligible`, `Protected`), preallocated per-service rings (`culprit_window / 5 s` deep), eligibility (research R5: `never_touch` ⇒ `Protected`; tgshd's own unit, `docker.service`, `containerd.service`, `dbus.service`, `ssh.service`, `sshd.service`, `systemd-*`, `user.slice`, `init.scope` never eligible), culprit = largest `current − minimum in culprit_window` of `anon`, at least `culprit_min_growth`, ties to the larger size (tests in pkg/health/memguard/culprit_test.go: US1-2 stable-large vs growing-small, sawtooth, ties, protected, core services, a service removed or renamed between samples — its ring is dropped and it cannot be culprit until it has new history) (FR-003, FR-004, R4, R5)
- [x] T008 [US1] Episode state machine in pkg/health/memguard/episode.go: `idle → danger → acted → idle | not_recovered`, `danger → reported` exactly as in data-model.md; danger = `AvailablePct < danger_available_percent` AND `FullAvg10 ≥ danger_pressure_percent` continuously for `danger_for`; `repeat_window` turns restart into stop; one outcome per episode for `not_recovered`, `protected`, `no_culprit`; `report` mode emits `would_*`; intervention records with `Export`/`Restore` dropping records older than `repeat_window` and never counting `report` records (depends on T007) (tests in pkg/health/memguard/episode_test.go: US1-1, US1-3, US1-4, US1-5, US1-6, US1-7, restore mid-episode, SC-004, SC-005) (FR-001, FR-002, FR-005, FR-006, FR-009, R8)
- [x] T009 [US1] Outcome texts in pkg/health/memguard/outcome.go: every guard outcome in contracts/messages.md "Guard outcomes", channel `mode` for `restarted` and every `would_*` (FR-001: a report-mode preview never wakes the operator) and `failed_mode` for the rest, including `protected` and `no_culprit` in report mode, key `memory/guard/<service>` (depends on T008) (tests in pkg/health/memguard/outcome_test.go: one case per table row, size formatting) (FR-007, FR-011)
- [x] T010 [US1] Guard loop in pkg/health/memguard/loop.go: `Clock`, `Sampler` (host snapshot every 1 s, services every 5 s), `Actuator` (restart/stop by kind) and `Reporter` interfaces; actions run in their own goroutine bounded by `action_timeout`, a timeout or unreachable runtime falls back to `Kill(cgroup)` and a `killed` outcome; outcomes and state exports go through buffered channels that drop the oldest and never block the loop; `intervene: off` ⇒ not started (depends on T009) (tests in pkg/health/memguard/loop_test.go on a fake clock: SC-001 replay 1 GB/min on 64 GB acts ≤ 60 s, SC-002 the same replay yields exactly one outcome for the intervention, on `mode` (Telegram), and none on `failed_mode` (no SMS), SC-006 a week-long replay of normal behaviour (stable large services, sawtooth caches) causes no intervention, US1-8 sampler slow on other paths does not delay, actuator hang ⇒ kill fallback, full report channel, no_culprit with top process, US1-9 unit restart, and `testing.AllocsPerRun` showing the steady-state sample/decide path makes no allocations) (FR-008, FR-010, FR-011, R3, R6, R10)
- [x] T011 [US1] Starvation shielding in pkg/host/procmem/shield.go: write `-1000` to `/proc/self/oom_score_adj`; `mlockall(MCL_CURRENT|MCL_FUTURE|MCL_ONFAULT)` via `syscall.Syscall(syscall.SYS_MLOCKALL, …)` with a local `mclOnFault = 4`; errors returned for the caller to raise as checker errors (tests in pkg/host/procmem/shield_test.go: oom_score_adj against a temp file; mlock path behind an injectable syscall func) and add `OOMScoreAdjust=-1000` and `LimitMEMLOCK=infinity` to config/tgshd.service (FR-008, R7)
- [x] T012 [US1] Wire the guard in cmd/health_memory.go and cmd/health.go: sampler over procmem + cgroupmem with the container id→name map (refreshed from the Docker list by the memory check, read through an atomic pointer), actuator over docker + systemctl + cgroupmem.Kill, reporter → `Runner.PushMode` and the alert log, guard state into the checkpoint as kv `memory.guard` and restored at start, shielding at start (failures as checker errors), start/stop on the fx lifecycle; no cgroup v2 ⇒ checker error and no guard (depends on T010, T011, T001–T006) (tests in cmd/health_test.go: guard started only with the memory section and `intervene ≠ off`; state round trip through the fake store; id→name mapping; the guard keeps acting while a maintenance/quiet window is open) (FR-001, FR-008, FR-009, R11)

**Checkpoint**: US1 delivers the protection on its own; deploy in `report` mode.

---

## Phase 3: User Story 2 - Early warning before memory runs out (Priority: P2)

**Goal**: Warnings on available memory, swap, pressure and steady growth, naming the top consumer; visible in `/status` and `/history`.

**Independent Test**: quickstart.md "Memory check" table.

- [x] T013 [P] [US2] Pure memory check in pkg/health/check/memory/memory.go: `Source` interface (host snapshot, services with `anon`, top process), conditions `memory/available`, `memory/swap` (only when `SwapTotal > 0`), `memory/pressure` (`SomeAvg60`) with texts from contracts/messages.md "Warnings", the top consumer in every PROBLEM, samples `memory.available_pct`, `memory.swap_pct`, `memory.pressure_some_avg60`; missing PSI ⇒ one `health.CheckError` and the other judgements continue (tests in pkg/health/check/memory/memory_test.go: US2-1, US2-3, no swap, missing PSI) (FR-012, FR-014, FR-015)
- [x] T014 [US2] Growth ring and judgement in pkg/health/check/memory/growth.go: one point per run, `growth_window` deep; PROBLEM `memory/growth/<service>` when rise ≥ `growth_warn` AND minimum precedes maximum AND latest ≥ 90 % of the window maximum; resolves below half of `growth_warn` or when the service is gone; `Export`/`Restore` as kv `memory.growth`, dropping services gone longer than `growth_window` (depends on T013) (tests in pkg/health/check/memory/growth_test.go: US2-2, SC-003 20 GB/h warns ≥ 30 min before 5 %, US2-5 sawtooth and SC-006 flat db, restore) (FR-013, FR-017, R12)
- [x] T015 [US2] Wire the memory check in cmd/health_memory.go and cmd/health.go (source over procmem + cgroupmem + Docker name map refresh, growth state through the checkpoint) and add the `/status` memory block of contracts/messages.md to pkg/health/report/status.go and the memory line of the weekly summary to pkg/health/report/summary.go, with their inputs in cmd/health_reports.go (depends on T013, T014, T012) (tests in pkg/health/report/status_test.go: memory block, guard line; pkg/health/report/summary_test.go: memory line; cmd/health_test.go: check built only with the section) (FR-015, FR-016)

**Checkpoint**: US1 + US2 work independently.

---

## Phase 4: User Story 3 - Containers without a memory limit are pointed out (Priority: P3)

**Goal**: One problem per running container without a memory limit, except exclusions.

**Independent Test**: quickstart.md "Memory check" row US3.

- [x] T016 [US3] Limit audit in pkg/health/check/memory/audit.go: `memory/limit/<container>` opens for a running container with limit 0 not in `audit_exclude`, resolves when it runs with a limit or stops; the check's `Source` gains containers with `HasLimit` from the Docker adapter (T004) in cmd/health_memory.go (depends on T013) (tests in pkg/health/check/memory/audit_test.go: US3-1, US3-2, US3-3, stopped container) (FR-018)

---

## Phase 5: User Story 4 - Out-of-memory kills are reported clearly (Priority: P3)

**Goal**: Each OOM kill is reported with process, service, size and cause, for this boot and the previous one.

**Independent Test**: quickstart.md "Kernel" table.

- [x] T017 [US4] Kernel `kernel/oom` class in pkg/health/check/kernel/kernel.go (and a new pkg/health/check/kernel/oom.go): pair the `oom-kill:constraint=…,task_memcg=…,task=…,pid=…` record with the following `Out of memory: Killed process` / `Memory cgroup out of memory: Killed process` record; one event per kill with the texts of contracts/messages.md "Kernel" (`CONSTRAINT_MEMCG` ⇒ "hit its memory limit"); service resolved through a `ServiceResolver` interface; at most 5 per run then "… and N more"; previous-boot records prefixed as in 002; resolver adapter over the Docker name map in cmd/health_hardware.go (tests in pkg/health/check/kernel/oom_test.go with synthetic kmsg and journal lines: US4-1, US4-2, US4-3, unpaired lines, burst cap; SC-007) (FR-019, R13)

---

## Phase 6: Polish & Cross-Cutting Concerns

- [ ] T018 [P] Document the feature: `memory` section in config/health.example.yaml (anonymised names), a "Memory protection" section in README.md (report → act rollout, never-touch list, unit-file lines, cgroup v2/PSI requirements), package-map rows for `pkg/health/memguard`, `pkg/health/check/memory`, `pkg/host/procmem`, `pkg/host/cgroupmem`, `pkg/host/systemctl` in .claude/CLAUDE.md (tests: pkg/health/config/config_test.go loads config/health.example.yaml) (FR-020)
- [ ] T019 `check --once` prints the memory readings and the guard mode in cmd/cli.go (tests in cmd/cli_test.go) (FR-016)
- [ ] T020 Run the quickstart.md validation: `task lint`, `task test:coverage` (80 % floor), `CGO_ENABLED=0 task build`, the private-data scan of the diff, and record the manual host run's outcome in specs/003-memory-protection/quickstart.md (SC-001, SC-002)

---

## Dependencies & Execution Order

- **Phase 1** (T001–T006) blocks everything; T001–T005 are parallel, T006 is independent of them.
- **US1** (T007 → T008 → T009 → T010; T011 parallel to T007–T010; T012 last) — the MVP.
- **US2** (T013 → T014 → T015) needs Phase 1; T015 shares cmd/health_memory.go with T012, so it follows T012.
- **US3** (T016) needs T013.
- **US4** (T017) needs only Phase 1 (T004 for the name map) and is otherwise independent.
- **Polish** after the stories it documents.

## Parallel Example

```text
Phase 1: T001, T002, T003, T004, T005 together; then T006.
US1:     T007 and T011 together; then T008 → T009 → T010 → T012.
After US1: T013 (US2) and T017 (US4) together.
```

## Implementation Strategy

1. Phase 1, then US1 → deploy with `intervene: report`; watch the "would restart" reports.
2. US2 and US4 next (warnings and clear OOM reports), then US3.
3. Switch the host to `intervene: act` once the reports have picked the right services.
