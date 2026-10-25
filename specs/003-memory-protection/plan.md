# Implementation Plan: Memory Protection

**Branch**: `003-memory-protection` (work is committed on `main`, per the constitution) | **Date**: 2026-10-25 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `specs/003-memory-protection/spec.md`

## Summary

Protect the host from runaway memory with approach **B (hybrid)**, chosen with the operator:

1. A **`memory` check** in the existing 30 s health tick: warnings on available memory, swap and
   pressure; per-service growth over an hour; an audit of containers without a memory limit;
   `/status`, `/history` and summary integration.
2. A **memory guard**: a goroutine started next to the health runner but never scheduled by it.
   It polls kernel files (`/proc/meminfo`, `/proc/pressure/memory`, cgroup v2 `memory.stat`) every
   1–5 s, detects danger (low available memory AND sustained full pressure), picks the service
   that grew the most, and restarts it — or stops it if it is a repeat offender — through Docker
   or `systemctl`, falling back to the kernel's `cgroup.kill` when the runtime does not answer.
   It runs with locked memory and OOM protection and never waits on the store, the network or
   Telegram. Default `intervene: report` previews actions until the operator switches to `act`.
3. The **kernel check** gains an out-of-memory class that names the killed process and service.

Rejected: **A** (everything in the tick — under thrashing the intervention would queue behind
other checks and miss its deadline, as on the night that motivated the feature); **C** (delegate
to `systemd-oomd`/kernel — cannot express growth-based victim choice, restart-not-stop or a
never-touch list, and oomd kills rather than restarts). Details: [research.md](research.md) R1.

## Technical Context

**Language/Version**: Go (toolchain in `go.mod`), `CGO_ENABLED=0` static binary

**Primary Dependencies**: existing and approved only — uber/fx, zap, telebot, `modernc.org/sqlite`,
`github.com/moby/moby/client`, `go.yaml.in/yaml/v3`. **No new third-party dependency**: kernel
files via `os`, `mlockall` via `syscall.Syscall` (R7), `systemctl` via `os/exec` (R6).
`golang.org/x/sys` stays indirect (R3).

**Storage**: the existing SQLite store. No migration: guard records and growth history are JSON in
`kv` (`memory.guard`, written by the guard itself through `Store.PutKV`; `memory.growth`); host metrics use the `samples` table (R11).

**Testing**: standard library `testing`, table-driven; hand-written fakes for `memguard.Clock`,
`Sampler`, `Actuator`, `Reporter`, the check's `Source`, and the kernel `ServiceResolver`;
synthetic `/proc` and cgroup trees in `t.TempDir()` for adapter tests; synthetic kmsg and journal
lines. Guard scenarios run on a fake clock, never real sleeps.

**Target Platform**: Linux host with cgroup v2 (systemd cgroup driver for Docker), PSI enabled,
running as root under systemd. Without PSI the pressure judgements are skipped (checker error);
without cgroup v2 the guard is not started (checker error); the daemon still starts (Principle V).

**Project Type**: daemon (unchanged)

**Performance Goals**: guard detection latency ≤ 2 s after both danger signals hold for
`danger_for`; action issued ≤ 60 s after danger holds (SC-001); guard steady-state cost ≤ 1 % of
one CPU (one `/proc` read per second, ~60 small cgroup reads every 5 s).

**Constraints**: guard decision path allocation-free in steady state (preallocated rings); no
store, network or Telegram on the decision path; messages ≤ 4 096 UTF-16 units; no real host data
in the repository (Principle VI).

**Scale/Scope**: one host; ~100 services (containers + units), ~500 processes.

No item is marked NEEDS CLARIFICATION: the spec's clarifications are settled, and every technical
unknown is resolved in [research.md](research.md) R1–R14.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

Constitution v1.0.0 (`.specify/memory/constitution.md`).

| Principle | Gate | Status | Evidence |
|---|---|---|---|
| I. Idiomatic Go, stdlib first | stdlib for every mechanism; focused packages behind small interfaces | PASS | kernel files via `os`; `mlockall` via `syscall`; `systemctl` via `os/exec`; new adapters `pkg/host/procmem`, `pkg/host/cgroupmem`, `pkg/host/systemctl` each do one job |
| II. Test-first, stdlib testing | decision logic pure and inside the gate; no test deps | PASS | `pkg/health/memguard` (episode machine, culprit, eligibility, outcomes), `pkg/health/check/memory` (warnings, growth, audit) and the kernel OOM parser are pure and under `./pkg/health/...`; the guard loop uses injected clock, sampler, actuator, reporter; `COVERPKG` unchanged |
| III. Dependency discipline | no new dependency, no indirect promoted | PASS | none; `x/sys` not promoted (R3) |
| IV. Static, CGO-free | `CGO_ENABLED=0` | PASS | `syscall.Syscall(SYS_MLOCKALL, …)` is pure Go |
| V. Env-gated; daemon always starts | absent memory section ⇒ nothing runs; missing PSI/cgroup v2/Docker degrades | PASS | memory runs only with its config section under the existing `BOT_HEALTH_CONFIG` gate; failures are checker errors |
| VI. Public repository hygiene | no real host data | PASS | examples use `web-app`, `db`, `worker`, `backup.service`; fixtures synthetic |
| VII. Commit discipline | plain subjects on `main`, push on request | PASS | |
| Engineering constraints | transport limits; no secrets | PASS | messages bounded by the existing truncation; no command output in texts |

**Post-design re-check**: PASS — the design adds an always-on goroutine with root-level effects
(restart/stop/kill). It is gated by the config section, defaults to `report`, never acts on core
services or tgshd itself, and every action is reported (FR-001, FR-004, FR-007).

## Project Structure

### Documentation (this feature)

```text
specs/003-memory-protection/
├── plan.md              # this file
├── research.md          # R1–R14
├── data-model.md        # snapshot, service, samples, episode machine, records, conditions
├── quickstart.md        # validation scenarios (fixtures and fakes; one manual host run)
├── contracts/
│   ├── config.md        # memory section + systemd unit additions
│   └── messages.md      # warnings, guard outcomes, kernel OOM, /status block
├── checklists/requirements.md
└── tasks.md             # /speckit-tasks output
```

### Source Code (repository root)

New files marked `+`.

```text
pkg/health/
├── memguard/ +                  # pure: Config, Service, Sample, rings, eligibility, culprit choice,
│   ├── guard.go +               #   episode machine (Observe → Decision), outcomes, Export/Restore
│   ├── culprit.go +
│   ├── episode.go +
│   └── loop.go +                # the guard goroutine: clock/sampler/actuator/reporter interfaces,
│                                #   bounded actions with cgroup.kill fallback, non-blocking report channel
├── check/memory/ +              # pure: warnings, growth ring (R12), limit audit, readings, samples,
│   └── memory.go +              #   status data; Export/Restore of the growth ring
├── check/kernel/kernel.go       # + kernel/oom class and formatter (R13), ServiceResolver
├── config/types.go, checks.go,  # + Memory section, Size type, defaults and validation
│   defaults.go, validate.go
├── report/status.go             # + memory block
└── runner/runner.go             # + PushMode (event with a delivery mode)

pkg/host/
├── procmem/ +                   # /proc/meminfo, /proc/pressure/memory, /proc/*/status top processes,
│                                #   oom_score_adj and mlockall(MCL_ONFAULT) helpers
├── cgroupmem/ +                 # walk system.slice, read memory.stat/current, write cgroup.kill,
│                                #   own cgroup from /proc/self/cgroup
├── systemctl/ +                 # exec systemctl restart|stop <unit>, bounded
└── docker/docker.go             # + Stop, container ID, HostConfig.Memory (limit) in Container

cmd/
├── health_memory.go +           # wiring: sources, name map (Docker list → id→name, refreshed by the
│                                #   memory check), guard start/stop on the fx lifecycle, reporter → PushMode,
│                                #   guard writer: kv `memory.guard` via Store.PutKV after each action and on stop;
│                                #   guard records restored from kv, not saved (guardPersister over runner.KVPersister)
├── health.go                    # build the memory check; restore guard records, restore/export growth state
└── cli.go                       # check --once: memory readings and guard mode

config/
├── health.example.yaml          # + memory section
└── tgshd.service                # + OOMScoreAdjust=-1000, LimitMEMLOCK=infinity
README.md                        # memory protection section
.claude/CLAUDE.md                # package map rows for memguard, check/memory, procmem, cgroupmem, systemctl
```

**Structure Decision**: Same layout as 001/002 — pure judgement in `pkg/health/**` (inside the
coverage gate), thin I/O in `pkg/host/**`, wiring in `cmd/health*.go`. The guard's loop lives in
`pkg/health/memguard` with every I/O behind an interface, so its timing and fallback behaviour are
unit-tested on a fake clock.

### Phasing (input for `/speckit-tasks`)

1. **Foundational**: config section and `Size` type; `procmem` and `cgroupmem` adapters; Docker
   `Stop`/ID/limit; `Runner.PushMode`.
2. **US1 (P1) guard**: pure engine (eligibility, culprit, episode, outcomes, export/restore); loop
   with actuator fallback and reporter; `systemctl` adapter; wiring, unit-file additions, mlock and
   OOM-score shielding.
3. **US2 (P2) warnings**: memory check (available, swap, pressure, growth ring), samples, `/status`.
4. **US3 (P3) audit**: limit audit conditions.
5. **US4 (P3) kernel OOM**: class, pairing parser, resolver, previous boot.
6. **Polish**: example config, README, CLAUDE.md package map, `check --once`, quickstart host run.

## Complexity Tracking

No constitution violation. Notable choices:

| Choice | Why | Simpler alternative rejected because |
|---|---|---|
| Guard as a separate goroutine with its own sampling | must act while the tick starves (FR-008) | in-tick intervention missed its deadline on the motivating night |
| `cgroup.kill` fallback | frees memory when Docker/systemd cannot answer | waiting on a starved daemon leaves the leak running |
| `mlockall(MCL_ONFAULT)` + `OOMScoreAdjust=-1000` | tgshd itself stalled on page faults that night | without it the guard can be swapped out exactly when needed |
