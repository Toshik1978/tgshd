# Implementation Plan: Deeper Health Checks

**Branch**: `002-deeper-health-checks` (work is committed on `main`, per the constitution) | **Date**: 2026-10-16 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `specs/002-deeper-health-checks/spec.md`

## Summary

Deepen the 001 host health monitoring so tgshd can be the host's only monitor. The design keeps
001's architecture unchanged: pure decision logic in `pkg/health/**` (inside the coverage gate),
thin I/O adapters in `pkg/host/**`, wiring in `cmd/health*.go`. Drives gain a larger built-in
counter set, a spare floor, SATA SSD wear, calendar self-test age with a persisted "last passed
newly observed", gap-aware reads that never resolve a problem on a partial read, and one forced
read per day of a sleeping drive (at most one per run, never retried before the same age). Delivery gains per-distinct checker-error keys, Telegram failure
tracking with an email notice, an SMS fallback (RESOLVED by SMS for PROBLEMs that went by SMS),
re-raising or listing of undelivered PROBLEMs (never dropped to a log line), a store recovery
notice, hardware checks that run through quiet windows unless set to pause
(`pause_in_quiet_windows`) and a stuck-lock limit. The kernel check reads the previous boot's tail by exec'ing
`journalctl -o json`, matches firmware, filesystem and link-reset records, reports lost records and
every uncorrectable memory error. Temperatures judge one sensor per CPU package, listed board
sensors only, and never treat a failed read as "gone". Network becomes one condition with
no-route as router down; endpoint, DoH, command and freshness checks gain the rules of US5,
including an HTTP freshness source. The weekly summary shows every drive and service in one
message with a defined cut order. Two store migrations (`user_version` 3: migration 2 adds the queue `kind`, `check_name`, `key`;
migration 3 adds the queue `plain` and `since`) add message metadata to
the retry queue; everything else is JSON in existing columns. No new dependency. A foundational
phase (keyed checker errors, migration 2) comes first; six delivery phases then follow the user
stories in priority order, and a polish phase closes.

## Technical Context

**Language/Version**: Go 1.27 (`go.mod`), `CGO_ENABLED=0` static binary

**Primary Dependencies**: existing and approved only — uber/fx, gocron v2, zap, telebot v4,
`modernc.org/sqlite`, `github.com/moby/moby/client` (+ `github.com/moby/moby/api`),
`go.yaml.in/yaml/v3`, `golang.org/x/net` (DoH), pgx (gammu), go.nut, pro-bing, caarlos0/env.
**No new third-party dependency** (Principle III): the journal is read by exec'ing `journalctl`
(research R7), the missing-container case uses the moby client's existing list call instead of
promoting `github.com/containerd/errdefs` (R22). External host tools: `smartctl` (required for
disks, now also run with `-n never` for forced reads), `nvidia-smi` (optional), `journalctl`
(optional; absent ⇒ the previous-boot tail is skipped with a log line).

**Storage**: the 001 SQLite database; migration 2 adds `kind`, `check_name`, `key` to `queue`
(R28). Drive status and baselines extend the `baselines.data` JSON; channel health, SMS-sent
problems, undelivered problems, maintenance observation and freshness stamps live in `kv`
([data-model.md](data-model.md)).

**Testing**: standard library `testing`, table-driven, hand-written fakes for every source
interface (`disks.Source`, `temps.Hwmon`/`Nvidia`, new `temps.OpenState`, `kernel.EDACSource`/new
`JournalSource`, `network.GatewaySource`, `command.Runner`, the existing file `freshness.Source`
and the new `freshness.StampSource`, `notify.Sender`/`Mailer`);
anonymised fixtures under each package's `testdata/` (smartctl JSON, `journalctl -o json` lines,
mountinfo, hwmon trees in `t.TempDir()`); `httptest` for http, DoH and HTTP freshness; the store
against a temp dir; `-race` in CI.

**Target Platform**: Linux host (systemd), running as root; the Docker image keeps working without
`journalctl` (previous-boot tail skipped) and remains unsupported for hardware checks (001).

**Project Type**: daemon + CLI subcommands (unchanged)

**Performance Goals**: the 001 budgets hold — a tick's runs end within 40 s, deliveries within
30 s more. A forced read adds at most one 20 s smartctl run per sleeping drive per 24 h, bounded
by the run deadline (`health.Bound`), at most one forced read per disks run. The previous-boot
journal read happens once per boot at the first kernel run, bounded by `health.Bound` at
`health.MaxCheckTimeout` (20 s) and 200 000 records; a cut-off is reported once and not retried.

**Constraints**: no cgo; the daemon starts without a journal, Docker, Postgres, SMTP or SMS;
messages ≤ 4 096 UTF-16 units; command output, header values and URLs never in messages or logs;
no real host data anywhere in the repository.

**Scale/Scope**: one host; ~10 drives (built-in counters 11 ATA + 1 NVMe each), ~50 containers,
~20 endpoints, ~10 freshness and command checks; summary must fit all of them in one message.

No item is marked NEEDS CLARIFICATION: the spec's one clarification (RESOLVED by SMS) is settled,
and every technical unknown is resolved in [research.md](research.md) R1–R28.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

Constitution v1.0.0 (`.specify/memory/constitution.md`).

| Principle | Gate | Status | Evidence |
|---|---|---|---|
| I. Idiomatic Go, stdlib first | stdlib for every new mechanism; errors wrapped with `%w`; focused packages behind small interfaces | PASS | journal via `os/exec` + `encoding/json` (R7); mounts via `/proc/self/mountinfo` + sysfs (R27); regex via `regexp` (R8, R24); HTTP freshness via `net/http` (R26); new `health.CheckError` wraps its cause (R6); two new adapter packages with one job each, `pkg/host/journal` and `pkg/host/httpstamp` |
| II. Test-first, stdlib testing (NON-NEGOTIABLE) | every behaviour change test-first; no test deps; decision logic pure in the gate; `COVERPKG` unchanged and in sync | PASS | all judgement (counter sets, transport classification, self-test head alignment, forced-read decision, sensor selection, plausible range, kmsg gap counting, episode timing, channel health, SMS set, undelivered rule, network single condition, stamp parsing, mount aggregation, summary cut) sits in `pkg/health/**`; adapters (`pkg/host/journal`, `httpstamp`, `smartctl`, `hwmon`, `nvidia`, `edac`, `sysfs`, `execprobe`, `docker`, `pkg/doh`) only do I/O and return raw data, and cmd adapters map their sentinels onto the pure packages' own; [quickstart.md](quickstart.md) lists a test per acceptance scenario; `COVERPKG` already covers `./pkg/health/...`, so no edit to `Taskfile.yml` or `ci.yml` is needed |
| III. Dependency discipline | no unapproved new dependency, no indirect promoted | PASS | none added; `containerd/errdefs` deliberately not promoted (R22) |
| IV. Static, CGO-free | `CGO_ENABLED=0` | PASS | `journalctl` exec instead of cgo journal bindings (R7); nothing else touches native code |
| V. Env-gated optional features; daemon always starts | no new optional feature without gating; absent dependencies degrade | PASS | no new env var; health stays gated by `BOT_HEALTH_CONFIG`; absent journal ⇒ log line (R7); absent SMS ⇒ PROBLEMs stay queued, email notice only (R18); absent SMTP ⇒ Telegram failure logged (R17); schema migration runs inside the existing deferred store open, so a store that cannot open still degrades as in 001 |
| VI. Public repository hygiene | no real host data in specs, fixtures, tests | PASS | fixtures and examples use `TEST000…` serials, `Example SSD`, `example.org`, RFC 5737 addresses, `+10000000000`; journal fixtures are synthetic lines; the private-pattern scan is run before push (see below) |
| VII. Commit and workflow discipline | plain imperative subjects, on `main`, push only on request | PASS | tasks will commit per task on `main` with plain subjects; nothing is pushed by the plan |
| Engineering constraints | transport limits; no secrets in messages | PASS | summary cut order keeps one message (R27); command output and header secrets never in text (R24, R22, R26) |

No violation; Complexity Tracking lists notable choices only.

## Project Structure

### Documentation (this feature)

```text
specs/002-deeper-health-checks/
├── plan.md              # this file
├── research.md          # R1–R28
├── data-model.md        # entity and state deltas, schema v3
├── quickstart.md        # validation scenarios (fixtures and fakes only)
├── contracts/
│   ├── config.md              # amends 001 contracts/config.md
│   ├── telegram-commands.md   # amends 001 contracts/telegram-commands.md (commands, summary, pushed messages)
│   └── cli.md                 # amends 001 contracts/cli.md
├── checklists/requirements.md
└── tasks.md             # /speckit-tasks output
```

The 001 environment and heartbeat contracts are unchanged.

### Source Code (repository root)

Files touched, by package (new files marked `+`):

```text
cmd/
├── health.go                    # runnerOptions: PauseInQuiet per check, maintenance max hold, telegram_failing_after;
│                                #   newHealth binds the late-bound open-state adapter to the runner
├── health_hardware.go           # disks: smartctlSource (smartctl.Node → disks.Node, wake), logger, one-shot
│                                #   WithoutForcedRead; temps: config.Temperatures, hwmon/nvidia
│                                #   conversions, ErrNoDevices → temps.ErrNoGPUs, openState (late-bound IsOpen);
│                                #   kernel: edac Err conversion, journal source (ErrUnavailable → kernel.ErrNoJournal,
│                                #   ErrCapped → kernel.ErrJournalCapped)
├── health_services.go           # http: follow_redirects, auth_refusal, lazyDocker ErrNotFound → httpcheck.ErrContainerNotFound,
│                                #   dohResolver (doh errors → httpcheck sentinels); freshness: httpstamp source;
│                                #   command: output-capturing runner
├── health_adapters.go           # network: gateway adapter (sysfs.ErrNoDefaultGateway → network.ErrNoRoute)
├── health_reports.go            # /disks, /temp, weekly summary inputs (drive status, mounts, freshness kv, probe state)
└── cli.go                       # check --once: ASLEEP line, per-key errors, schema-v1 fallback

pkg/health/
├── check.go                     # + CheckError; Check.PauseInQuiet
├── engine.go                    # + Reopen
├── config/
│   ├── types.go, checks.go      # new keys (contracts/config.md)
│   ├── defaults.go              # changed defaults; per-section pause default; network threshold = interval
│   └── validate.go              # new validation rules
├── notify/
│   ├── notify.go                # QueuedMessage kind/check/key; channel health; SMS fallback; undelivered prefix
│   ├── checker.go               # email-only Telegram-failing notice
│   └── + channels.go            # ChannelHealth, SMS-sent set, undelivered list {Items, Folded} (pure state + kv codecs)
├── runner/
│   ├── runner.go                # quiet ticks run non-pausing checks; per-key checker errors; networkUnknown;
│   │                            #   KeyNetwork; OnDrop → Reopen/undelivered; store recovery notice;
│   │                            #   + IsOpen(key); Snapshot.Paused
│   ├── deliver.go               # Telegram-failing check after each tick's deliveries
│   ├── + maintenance.go         # lock observation and stuck limit
│   └── persist.go               # queue metadata; kv persisters for notify/maintenance state
├── store/
│   ├── store.go                 # migration 2
│   └── state.go                 # queue columns, validate kind
├── report/
│   ├── readings.go              # /disks and /temp lines
│   ├── status.go                # /status quiet note listing the paused checks
│   ├── + mounts.go              # DriveMounts: pure aggregation of mountinfo, partitions and holders
│   └── summary.go               # drive lines, services per check/group, freshness ages, modem probe, cut order
└── check/
    ├── disks/
    │   ├── smart.go             # value/rotation_rate decode, counter set arg, tri-state verdict, gaps,
    │   │                        #   self-test classification and entries, SATA wear
    │   ├── disks.go             # Source{Drives() []Node, Run(..., wake)}; counter names; spare floor; forced read
    │   │                        #   (forced_read_at, one per run); asleep/unreadable since; empty discovery;
    │   │                        #   transport filter and upgrade retire; per-serial temp limit, at-or-above
    │   ├── + transport.go       # ClassifyTransport(path)
    │   ├── + selftest.go        # head alignment, new failures, calendar age
    │   └── + status.go          # DriveStatus export
    ├── temps/temps.go           # Sensor.Device/Err, GPU *Celsius, ErrNoGPUs, OpenState; package selection,
    │                            #   board allow-list/crit, plausible range, unreadable vs gone
    ├── kernel/
    │   ├── kernel.go            # Counters.Err; class table, lost records, unreported episodes, ue every rise,
    │   │                        #   per-controller errors; WithJournal, WithLogger, ErrNoJournal, ErrJournalCapped
    │   └── + boottail.go        # previous-boot tail selection by usec, bounded read, incomplete error
    ├── network/network.go       # network/connectivity, ErrNoRoute ⇒ Bad, Forget former keys
    ├── httpcheck/
    │   ├── httpcheck.go         # follow_redirects, auth_refusal, ErrContainerNotFound ⇒ Bad
    │   ├── dial.go              # ErrNameNotFound / ErrNoAddress sentinels ⇒ Bad
    │   └── secret.go            # extra placeholders; header secret shared with freshness
    ├── command/command.go       # Runner returns output; output patterns, timeout_verdict
    └── freshness/
        ├── freshness.go         # future tolerance, stamp kv for summary
        └── + stamp.go           # new StampSource interface (the existing file interface is Source);
                                 #   header secret, status rule, body parsing (pure)

pkg/host/
├── smartctl/smartctl.go         # Node{Name, Path} (raw resolved sysfs path); Run(..., wake) with -n never; ForcedTimeout
├── hwmon/hwmon.go               # Sensor.Device, Sensor.Err for unreadable inputs
├── nvidia/nvidia.go             # ErrNoDevices; keep [N/A] GPUs (Celsius *float64)
├── edac/edac.go                 # per-controller Err
├── kmsg/kmsg.go                 # unchanged reader (gaps detected in the check)
├── sysfs/sysfs.go               # + MountInfo() raw /proc/self/mountinfo entries; + BlockTopology() partitions, holders
├── execprobe/execprobe.go       # capture output (64 KiB cap)
├── docker/docker.go             # ErrNotFound on inspect of a missing container
├── + journal/journal.go         # journalctl -o json reader, ErrUnavailable, ErrCapped
└── + httpstamp/httpstamp.go     # GET a stamp endpoint: status and at most 4 KiB of body, no redirects

pkg/doh/doh.go                   # ErrNXDomain; definitive negative answers
config/health.example.yaml       # new keys and changed defaults, commented
README.md                        # health section: new keys, quiet-window change, journal note
.claude/CLAUDE.md                # package map: journal and httpstamp under pkg/host/*
```

**Structure Decision**: single Go module, the 001 layout. New decision logic goes into existing
pure packages (three new files in `disks`, one in `kernel`, one in `freshness`, one in `report`, two
in `notify`/`runner`); the two new packages are thin adapters, `pkg/host/journal` and
`pkg/host/httpstamp`, outside the coverage gate like every `pkg/host/*` adapter. Pure packages
never import `pkg/host/*` or `pkg/doh`: each adapter sentinel is mapped onto the pure package's own
sentinel by a cmd adapter. The coverage gate scope (`./pkg/health/...`) already contains every new
pure file.

## FR → code map

| FR | Packages and files |
|---|---|
| FR-001 | `pkg/host/smartctl/smartctl.go` (raw path), `pkg/health/check/disks/transport.go` (`ClassifyTransport`), `disks.go` (filter, `include_serials`, upgrade retire), `pkg/health/config/types.go`, `cmd/health_hardware.go` |
| FR-002 | `pkg/health/check/disks/disks.go` (`judge`: empty discovery) |
| FR-003 | `pkg/host/smartctl/smartctl.go` (`wake`), `pkg/health/check/disks/disks.go` (forced read, `forced_read_at`, asleep since), `pkg/health/check/disks/status.go`, `pkg/health/report/readings.go`, `cmd/cli.go` |
| FR-004, FR-005 | `pkg/health/check/disks/smart.go` (verdict, gaps), `disks.go` (no reading + `CheckError`) |
| FR-006, FR-007 | `pkg/health/check/disks/smart.go` (counter set, err-log as sample), `disks.go` (`counterName`), `pkg/health/config/types.go` |
| FR-008 | `pkg/health/check/disks/disks.go` (`judgeNVMe`), config `spare_floor_percent` |
| FR-009 | `pkg/health/check/disks/smart.go` (attr 177, `rotation_rate`), `report/readings.go`, `report/summary.go` |
| FR-010, FR-011, FR-012 | `pkg/health/check/disks/smart.go` (classification, format), `+selftest.go`, `disks.go` |
| FR-013 | `pkg/health/check/disks/disks.go` (`judgeTemp`) |
| FR-014, FR-015 | `pkg/health/config/defaults.go`, `types.go`; `check/temps/temps.go`; `check/disks/disks.go` |
| FR-016 – FR-019 | `pkg/health/check/temps/temps.go`; `pkg/host/hwmon/hwmon.go`; `pkg/host/nvidia/nvidia.go`; `pkg/health/runner/runner.go` (`IsOpen`); `cmd/health_hardware.go`, `cmd/health.go` (late-bound `openState`) |
| FR-020 | `pkg/host/journal/journal.go`; `pkg/health/check/kernel/boottail.go`, `kernel.go` (cursor `usec`, bounded read); `cmd/health_hardware.go` |
| FR-021 | `pkg/health/check/kernel/kernel.go` (`classTable`) |
| FR-022, FR-023, FR-024 | `pkg/health/check/kernel/kernel.go` |
| FR-025 | `pkg/host/edac/edac.go`; `pkg/health/check/kernel/kernel.go`; `cmd/health_hardware.go` |
| FR-026 | `pkg/health/config/defaults.go`, `check.go`, `runner/runner.go` (`quietTick`, `Snapshot.Paused`), `report/status.go`, `cmd/health.go` |
| FR-027 | `pkg/health/runner/maintenance.go`, config `maintenance_max_hold` |
| FR-028 | `pkg/health/check.go` (`CheckError`), `runner/runner.go` (`apply`); every check returning errors |
| FR-029 | `pkg/health/notify/channels.go`, `notify.go`, `checker.go`; `runner/deliver.go` |
| FR-030 | `pkg/health/notify/notify.go`, `channels.go`; `store/state.go` + migration 2 |
| FR-031 | `pkg/health/notify/notify.go` (`OnDrop` with kind/key, prefix), `channels.go` (undelivered list), `engine.go` (`Reopen`), `runner/runner.go` |
| FR-032 | `pkg/health/runner/runner.go` (`checkpoint`) |
| FR-033 – FR-036 | `pkg/health/check/network/network.go`; `runner/runner.go` (`KeyNetwork`, `networkUnknown`); `config/defaults.go`; `cmd/health_adapters.go` |
| FR-037 – FR-039 | `pkg/health/check/httpcheck/httpcheck.go`, `secret.go`; `pkg/host/docker/docker.go`; `cmd/health_services.go` |
| FR-040 | `pkg/doh/doh.go`; `pkg/health/check/httpcheck/dial.go`; `cmd/health_services.go` (`dohResolver`) |
| FR-041 | `pkg/health/check/command/command.go`; `pkg/host/execprobe/execprobe.go`; `cmd/health_services.go` |
| FR-042, FR-043 | `pkg/health/check/freshness/freshness.go`, `+stamp.go`; `pkg/host/httpstamp/httpstamp.go`; `cmd/health_services.go` |
| FR-044 – FR-049 | `pkg/health/report/summary.go`, `+mounts.go`; `pkg/host/sysfs/sysfs.go` (`MountInfo`, `BlockTopology`); `cmd/health_reports.go` |

## Delivery phases (map to user stories)

The phases are those of [tasks.md](tasks.md). Each is shippable on its own, is one commit, and
ends with `task lint`, `task test:coverage` and the private-pattern scan green; every task inside a
phase also leaves the build green. There is no Setup phase (no new dependency, tool or package
outside `pkg/health/...` holds decisions, so `COVERPKG` needs no change).

| Phase | Story | Steps | FRs |
|---|---|---|---|
| 2 Foundational | — (blocks every story) | `health.CheckError` → per-key raising in the runner → store migration 2 (queue `kind`, `check_name`, `key`) → one-shot CLI per-key errors and schema-v1 fallback | FR-028; FR-030/FR-031 prerequisite (migration) |
| 3 | US1 Drives (P1) | config keys → drive source shape (raw path, `wake`) through smartctl, disks and cmd → `DecodeSMART` counter set, names and SATA wear → tri-state verdict, gaps, self-test classification → self-test head and calendar age → spare floor, wear, sections, temperature → self-test judgement → forced read (`forced_read_at`, one per run) → transport classification, empty discovery, upgrade retire → drive status export → `/disks` line → one-shot `ASLEEP` line | FR-001 – FR-013 |
| 4 | US2 Delivery and resolution (P2) | `Reopen` → quiet and delivery config keys → queue metadata → channel state and undelivered list → Telegram-failing email → SMS fallback → dropped PROBLEMs (three branches) → kv persistence → store recovery notice → quiet ticks run non-pausing checks → maintenance stuck limit and `/status` note → wiring | FR-026, FR-027, FR-029 – FR-032 |
| 5 | US3 Kernel and memory (P3) | journal adapter; EDAC per-controller errors through adapter, check type and cmd → class table → lost records → unreported episodes → `ue` every rise and unreadable controllers → previous-boot tail (bounded, incomplete error) → wiring | FR-020 – FR-025 |
| 6 | US4 Temperatures (P4) | config keys; hwmon `Device`/`Err` through adapter, check type and cmd → GPU `ErrNoDevices`/`[N/A]` likewise → drive limits at or above and per serial → package selection → board allow-list, crit, plausible range → `Runner.IsOpen` and the late-bound adapter → unreadable versus gone → `/temp` and wiring | FR-014 – FR-019 |
| 7 | US5 Network and services (P5) | network condition with no route (`ErrNoRoute`) and `KeyNetwork` → network threshold default and `networkUnknown` → service config keys; docker `ErrNotFound`; DoH answers with httpcheck sentinels and the cmd adapter; `httpstamp` adapter → redirects, missing container, auth, placeholders → NXDOMAIN/NODATA judged → command output (interface, adapter, cmd) → future stamps → HTTP stamp source (pure) → wiring | FR-033 – FR-043 |
| 8 | US6 Weekly summary (P6) | `sysfs` raw mountinfo and topology; `summary_group`; freshness stamp kv → `report.DriveMounts` → drive lines → services lines → cut order → wiring | FR-044 – FR-049 |
| 9 Polish | — | example config → README and CLAUDE.md → full gate, SC-012 traceability, scans | all (docs) |

Every FR is implemented in exactly one phase (prerequisites and later reuses are cited elsewhere, e.g.
FR-030/FR-031 for migration 2 in Phase 2, FR-009 for SATA wear in the summary): FR-028 (Phase 2); FR-001–013 (Phase 3); FR-026, 027,
029–032 (Phase 4); FR-020–025 (Phase 5); FR-014–019 (Phase 6); FR-033–043 (Phase 7); FR-044–049
(Phase 8). FR-028 belongs to US2 in the spec and is moved into the Foundational phase because the
drive checker errors of US1 depend on it; migration 2 sits there too because R28 serves every
story and the one-shot CLI must handle a version-1 database from the first upgraded build.

## Post-design Constitution Check

Re-checked after research, data model and contracts:

- **I**: unchanged PASS — the design added only standard-library mechanisms; `pkg/host/journal`
  and `pkg/host/httpstamp` are focused adapters that return raw data.
- **II**: unchanged PASS — every judgement listed in [data-model.md](data-model.md) is a pure
  function of fixtures/fakes and lives under `pkg/health/**`; each acceptance scenario has a named
  test in [quickstart.md](quickstart.md); `COVERPKG` needs no change.
- **III**: unchanged PASS — confirmed no import beyond `go.mod`'s current direct set; the
  containerd errdefs helper is not used.
- **IV**: unchanged PASS.
- **V**: unchanged PASS — the journal, SMTP, SMS and Docker absences each degrade with a log line
  or a checker error; the schema migration cannot stop the daemon (the deferred store retries an
  open that fails, as in 001).
- **VI**: PASS — the private-pattern scan and the banned-word scan were run over
  `specs/002-deeper-health-checks` with no hits.
- **VII**: unchanged PASS.

Result: **PASS**, no violations, Complexity Tracking needs no justification entries.

## Complexity Tracking

No constitution violations. Notable choices, for the reviewer:

| Item | Why Needed | Simpler Alternative Rejected Because |
|---|---|---|
| `journalctl` exec for the previous boot's tail (R7) | FR-020; journal readers in Go need cgo | reading journal files directly relies on an undocumented format; pstore covers only panics |
| Queue rows carry kind, check and key (migration 2, R28) | FR-030 and FR-031 need to know which queued messages are PROBLEMs and for which condition | parsing the rendered text back is fragile and breaks with the host prefix and HTML escaping |
| Forced read inside the same disks run (R4) | FR-003 with SC-003 (at most one wake per 24 h) | a separate daily wake job wakes drives that were read recently |
