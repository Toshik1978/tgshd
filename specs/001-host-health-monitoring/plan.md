# Implementation Plan: Host Health Monitoring

**Branch**: `001-host-health-monitoring` (work is committed on `main`, per CLAUDE.md) | **Date**: 2026-10-16 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `specs/001-host-health-monitoring/spec.md`

## Summary

Move host health monitoring into the tgshd daemon: a pure, table-tested alert engine
(streaks, thresholds counted over observed time, quiet windows, rate limits) drives ten check
types behind small source interfaces; state and history live in an embedded pure-Go SQLite
database with in-memory state as the source of truth; alerts go to Telegram, SMS (gammu) and,
for checker errors, email. New Telegram commands expose status, history and readings, and the
existing command surface gains a chat allow-list. Delivered in five phases mirroring the user
stories; the access filter is the first task.

## Technical Context

**Language/Version**: Go 1.27, `CGO_ENABLED=0` static binary

**Primary Dependencies**: existing — uber/fx, gocron v2, zap, telebot v4, pgx (gammu DB),
robbiet480/go.nut, pro-bing, caarlos0/env. New (approved) — `modernc.org/sqlite`,
`github.com/moby/moby/client`, `go.yaml.in/yaml/v3`. Promoted from indirect to direct (approved) —
`golang.org/x/net` (DoH message encoding). Approved but **not used** —
`github.com/shirou/gopsutil/v4` (research R5). External host tools — `smartctl` (required for
disk checks), `nvidia-smi` (optional).

**Storage**: SQLite file in the state directory (WAL, single connection, `user_version` migrations)
— [data-model.md](data-model.md)

**Testing**: standard library `testing`, table-driven, hand-written fakes for every source
interface; anonymised JSON/text fixtures under `testdata/`; `modernc.org/sqlite` against a temp
dir for the store; `-race` in CI

**Target Platform**: Linux host (systemd), running as root; the Docker image keeps working for
the existing features but is not supported for hardware checks

**Project Type**: daemon + CLI subcommands

**Performance Goals**: a full tick (all due checks) < 60 s with default timeouts; command replies
< 5 s (SC-007)

**Constraints**: no cgo; monitoring starts without Docker/Postgres; no real host data anywhere in
the repository; messages ≤ 4 096 chars

**Scale/Scope**: one host; ~10 drives, ~50 containers, ~20 endpoints; samples ≈ 100 metrics per
15 min ⇒ ~1 M rows / 90 days (tens of MB)

## Constitution Check

`.specify/memory/constitution.md` is still the unfilled template; the gates below are the
non-negotiable conventions of `CLAUDE.md`.

| Gate | Status | Notes |
|---|---|---|
| Idiomatic Go, stdlib first, errors wrapped with `%w` | PASS | hwmon, sysfs, kmsg, flock, SMTP, HTTP all stdlib; gopsutil and ghw dropped |
| Tests: stdlib `testing`, hand-written fakes, no test deps | PASS | every adapter behind an interface; fixtures, no mocks library |
| New third-party deps need approval | PASS | sqlite, moby client, yaml approved in brainstorming; `golang.org/x/net` promotion approved 2026-10-16 |
| `CGO_ENABLED=0` | PASS | go-nvml, mattn/go-sqlite3, sdjournal rejected for cgo |
| Optional features env-gated in `main()` | PASS | `BOT_HEALTH_CONFIG` gates `healthProviders()`; SMTP gated by `BOT_SMTP_ADDR`; gammu inbox by `BOT_GAMMU_DSN` |
| 80 % coverage on cleanly testable packages | PASS | `./pkg/health/...` added to `COVERPKG`; parsers sit in check packages, adapters stay thin and outside the gate |
| Commit style, work on `main` | PASS | plain imperative subjects |
| Public-repo rule (spec FR-041) | PASS | fixtures anonymised (serials `TEST0000…`, models `Example SSD`), docs use RFC 5737 / `example.org` |

Re-checked after Phase 1 design: unchanged.

## Project Structure

### Documentation (this feature)

```text
specs/001-host-health-monitoring/
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── contracts/           # config.md, telegram-commands.md, cli.md, heartbeat.md, environment.md
├── checklists/requirements.md
└── tasks.md             # /speckit-tasks
```

### Source Code (repository root)

```text
cmd/
├── main.go                     # + subcommand dispatch (check --once, heartbeat), healthProviders()
└── app/                        # unchanged Worker/Application

pkg/
├── telegram/
│   ├── consumer.go             # + allow-list filter before dispatch (FR-044)
│   └── access.go               # allowed-set parsing/check (pure, tested)
├── command/                    # + status, history, temp, disks, retire, clear handlers
├── health/                     # PURE: Reading, Condition, Engine (R1), quiet gate, rate marks, events
│   ├── config/                 # YAML load + per-entry validation (R10)
│   ├── notify/                 # routing tlg/sms/tlg-or-sms, host prefix, HTML escape, queue, checker errors (R11, R12)
│   ├── runner/                 # health Worker: tick, network-first, deps, quiet windows, heartbeat, daily/weekly jobs (R2, R13)
│   ├── report/                 # text for /status, /history, /temp, /disks, weekly summary (pure)
│   ├── store/                  # SQLite: schema, migrations, checkpoint, queries, retention (R9)
│   └── check/
│       ├── network/            # gateway + internet targets
│       ├── ups/                # NUT freshness
│       ├── disks/              # smartctl JSON parse, baselines, self-tests, missing/new (R3, R4)
│       ├── temps/              # hwmon + GPU (R5); drive temps are judged by disks/
│       ├── kernel/             # kmsg classification + EDAC (R6)
│       ├── containers/         # inspect deltas (R7)
│       ├── httpcheck/          # status range, connect_to, DoH, header from env (R8)
│       ├── freshness/
│       ├── command/            # probe + fix action
│       └── modemprobe/         # weekly round-trip (R14)
├── host/                       # THIN I/O ADAPTERS (outside coverage gate)
│   ├── smartctl/               # exec smartctl, list /sys/block
│   ├── hwmon/                  # read /sys/class/hwmon
│   ├── nvidia/                 # exec nvidia-smi
│   ├── kmsg/                   # /dev/kmsg reader, boot id
│   ├── edac/                   # sysfs counters
│   ├── sysfs/                  # uptime, default gateway (/proc/net/route)
│   ├── lockfile/               # maintenance probe, instance lock, heartbeat file
│   └── docker/                 # moby client wrapper (+ restart for fix actions)
├── mail/                       # net/smtp sender
├── doh/                        # RFC 8484 resolver (x/net/dns/dnsmessage)
├── power/                      # + Status(): ups.status, charge, voltage, freshness errors
├── ping/                       # reused for network check
└── sms/
    ├── inbox.go                # incoming SMS worker: grouping, forwarding, probe replies
    └── gammu/                  # + inbox repository (select unprocessed, mark processed)
```

**Structure Decision**: Single Go module, extending the existing `cmd/` + `pkg/` layout.
`pkg/health/**` holds all decision logic with no I/O (in the coverage gate). `pkg/host/**`,
`pkg/mail`, `pkg/doh` and the docker wrapper are thin adapters (outside the gate, like today's
`ping`/`power`/`zte`). Each check package defines the small source interface it consumes; fx
wires adapters to those interfaces in `healthProviders()`.

## Delivery phases (map to user stories)

1. **Core (US1)**: access filter → engine → config → store → notify (+ email) → runner (quiet
   windows, heartbeat, instance lock) → network + ups checks → `/status`, `/clear` → `check --once`,
   `heartbeat` CLI → fx wiring, docs.
2. **Hardware (US2)**: smartctl/sysfs adapters → disks check → hwmon/nvidia adapters → temps
   check → kmsg/edac adapters → kernel check → `/retire`.
3. **Services (US3)**: docker adapter → containers check → doh + httpcheck → freshness → command
   check with fix actions.
4. **SMS (US4)**: gammu inbox repository → inbox worker → modem probe.
5. **History & reports (US5)**: samples + retention/roll-up → `/history`, `/temp`, `/disks` →
   weekly summary.

## Complexity Tracking

| Item | Why Needed | Simpler Alternative Rejected Because |
|---|---|---|
| `golang.org/x/net` becomes a direct dependency (approved) | RFC 8484 DoH needs DNS wire messages (`dns/dnsmessage`) | Hand-written DNS encoding is ~150 lines of protocol code; the JSON DoH API is provider-specific |
| Long-lived `/dev/kmsg` goroutine outside the tick | Kernel records arrive continuously; the ring buffer can overwrite between ticks | Polling `dmesg` every tick scrapes text and can miss records |
