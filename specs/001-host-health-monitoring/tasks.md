---

description: "Task list for host health monitoring"
---

# Tasks: Host Health Monitoring

**Input**: Design documents from `specs/001-host-health-monitoring/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md

**Tests**: Every task is implemented test-first (standard library `testing`, table-driven,
hand-written fakes, anonymised fixtures under `testdata/`). There are no standalone test tasks:
each task writes its own tests and is done only when they pass with `-race`.

**Organization**: Setup and Foundational phases, then one phase per user story in priority order.

**Global rules for every task**

- No real host data in code, tests, fixtures or docs: serials like `TEST0000000001`, models like
  `Example NVMe SSD`, addresses from RFC 5737, hosts under `example.org`.
- Wrap errors with `%w`; follow the style of the file being edited.
- Adapters (exec, sysfs, sockets) stay thin; parsing and decisions live in `pkg/health/**`.
- Run `task lint` and `task test:coverage` before closing a task.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (US1–US5)

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Coverage gate and package skeleton

- [x] T001 Extend the coverage gate in Taskfile.yml: add `./pkg/health/...` to `COVERPKG` and update its comment (adapters under `pkg/host/**`, `pkg/mail`, `pkg/doh` stay outside the gate); create `pkg/health/doc.go` with the package comment describing the pure engine boundary

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Access control, the alert engine, configuration, storage, notification and the
runner that every user story plugs checks into

**⚠️ CRITICAL**: No user story work can begin until this phase is complete

- [ ] T002 Telegram allow-list (FR-044, research R15): add `pkg/telegram/access.go` (parse `TELEGRAM_CHAT_ID` ∪ comma-separated `TELEGRAM_ALLOWED_IDS`; `Allowed(chatID int64) bool`), drop every update whose `Chat.ID` is not allowed in `pkg/telegram/consumer.go` before dispatch (both `/cmd` handlers and the channel-post path) with a warn log carrying chat and sender IDs and no reply; add `AllowedIDs []int64 env:"TELEGRAM_ALLOWED_IDS" envSeparator:","` in cmd/main.go; document the variable in README.md
- [ ] T003 [P] Alert engine core in pkg/health/engine.go (data-model "Reading", "Condition", state transitions; research R1): `Verdict` Good/Bad/Unknown, `Kind` State/Event, `Condition{Key, Check, StreakStart, BadFor, LastReadingAt, LastText, Open, OpenedAt}`, `Engine.Apply(now, check, interval, threshold, readings) []Event`
    - BadFor += min(now − LastReadingAt, 1.5 × interval); PROBLEM when BadFor ≥ threshold (threshold 0 ⇒ first Bad); Good resets and emits RESOLVED if Open; Unknown changes nothing and does not advance LastReadingAt
    - Event readings (Kind=Event) emit one event each, no state
    - `Clear(key)` returns to Healthy with no message
- [ ] T004 [P] Quiet gate and rate marks in pkg/health/quiet.go and pkg/health/ratelimit.go: `Quiet(maintHeld bool, uptime, grace time.Duration) (bool, reason)`; `RateMarks` keyed by `key+channel` with `Allow(now, window)`; used for checker errors (24 h) and kernel "still happening" (6 h)
- [ ] T005 [P] Health configuration loader in pkg/health/config/config.go per contracts/config.md (research R10): add `go.yaml.in/yaml/v3`; strict decoding (`KnownFields(true)`); durations accept Go syntax plus `d`; defaults for every section; per-entry validation returning `[]EntryError{File, Entry, Reason}` while keeping valid entries; names `[a-z0-9-]+`; "Thresholds ≥ 0; intervals ≥ 30 s (the tick); max ages > 0; status ranges within 100–599"; `connect_to` and `doh` mutually exclusive; `doh` implies `needs_internet: true`
- [ ] T006 [P] SQLite store open and schema in pkg/health/store/store.go (research R9, data-model "SQLite schema"): add `modernc.org/sqlite`; open `<state_dir>/health.db` with WAL, `busy_timeout`, `SetMaxOpenConns(1)`; migrations keyed by `PRAGMA user_version`; create the v1 tables and indexes verbatim from data-model.md; `OpenReadOnly` for `check --once`
- [ ] T007 Store load and checkpoint in pkg/health/store/state.go: `Load() (State, error)` and `Checkpoint(dirty State, newAlerts, newSamples) error` in one transaction for conditions, baselines, kv, queue, alerts, samples; on write error enter degraded mode (return a sentinel error once, keep accepting dirty data, retry each call), first success flushes everything (FR-013)
- [ ] T008 [P] Notifier in pkg/health/notify/notify.go (research R11, contracts/telegram-commands.md "Alert messages"): pushed messages only (never command replies) get the host prefix (FR-010): events → `<host>: PROBLEM: <text> (since HH:MM)` / `RESOLVED` / event text; HTML-escape; truncate to "≤ 4 096 characters after the host prefix (truncate with `…`)"; modes `tlg`, `sms`, `tlg-or-sms`; network PROBLEM by SMS and its RESOLVED by tlg-or-sms; failed Telegram sends queued and retried by `Flush`; queued silently while `Offline` is set; `sms` without an SMS sender degrades to `tlg` plus one checker error
- [ ] T009 Checker errors in pkg/health/notify/checker.go (FR-045, research R12): `CheckerError(key, text)` always logs; sends `<host>: CHECKER ERROR: <check>: <text>` to Telegram and to the email channel when configured; each key at most once per 24 h per channel using RateMarks; records an alert of kind `checker_error`
- [ ] T010 [P] SMTP sender adapter in pkg/mail/mail.go: stdlib `net/smtp`, STARTTLS when offered, optional PLAIN auth, `From`/`To`/`Subject`/`Date` headers; config from `BOT_SMTP_ADDR`, `BOT_SMTP_FROM`, `BOT_SMTP_TO`, `BOT_SMTP_USER`, `BOT_SMTP_PASS` (contracts/environment.md); test message building with a fake dialer
- [ ] T011 [P] Host file adapters in pkg/host/lockfile/lockfile.go and pkg/host/sysfs/sysfs.go (research R13): maintenance probe (`flock LOCK_SH|LOCK_NB`, released at once; missing file ⇒ not held), exclusive instance lock on `<state_dir>/tgshd.lock`, heartbeat read/write per contracts/heartbeat.md (sidecar `<path>.lock`, temp file + rename, "`planned_until` window 0–86 400 s"); uptime from `/proc/uptime` and default gateway from `/proc/net/route` with parsers tested on literal fixtures
- [ ] T012 Runner in pkg/health/runner/runner.go and the check contract in pkg/health/check.go (research R2): `Check{Name, Interval, Threshold, Mode, NeedsInternet, DependsOn, Run(ctx) CheckResult}`; `health` app.Worker with a 30 s tick that writes the heartbeat first, returns inside quiet windows, runs `network` first when due, sets/clears notifier Offline, skips `needs_internet` checks while offline and checks whose `depends_on` condition is bad, runs the rest with bounded concurrency, applies the engine, dispatches events, flushes the queue, checkpoints the store, and exposes a mutex-guarded `Snapshot()`, `Clear(key)` and `Retire(serial)` for command handlers
- [ ] T013 Due computations in pkg/health/runner/schedule.go: per-check "due" from last run and interval, daily job once per local day, weekly "weekday + HH:MM" due exactly once per week even across restarts (last-run stored in kv)

**Checkpoint**: Foundation ready — checks can be plugged in

---

## Phase 3: User Story 1 - Trustworthy alerts that survive restarts (Priority: P1) 🎯 MVP

**Goal**: Network and UPS alerts with the noise rule, quiet windows, persistence, heartbeat,
`/status`, `/clear`, and the one-shot and heartbeat CLI

**Independent Test**: spec US1 — outage longer than threshold ⇒ one SMS PROBLEM, one RESOLVED,
no duplicate after restart, silence during maintenance, heartbeat always fresh

- [ ] T014 [P] [US1] Network check in pkg/health/check/network/network.go: gateway from a `GatewaySource`, three pings via the existing `command.Pinger`-style interface (any reply ⇒ up), internet up when any `internet_targets` answers; readings `network/router` and `network/internet` (internet not judged while the router is down); a gateway lookup failure or missing ping capability is Unknown
- [ ] T015 [P] [US1] UPS freshness: add `Status(ctx)` to pkg/power/power.go returning `ups.status`, `battery.charge`, `input.voltage` with distinct errors for stale data, driver not connected, connection failure, empty answer and timeout; check in pkg/health/check/ups/ups.go judging all of those as Bad (a hang counts as Bad, FR-032) and emitting `ups.charge`/`ups.voltage` samples
- [ ] T016 [US1] `/status` and `/clear` in pkg/health/report/status.go, pkg/command/command_status.go and pkg/command/command_clear.go per contracts/telegram-commands.md: open problems with text and since, or the all-OK line with check count and last cycle; notes degraded store or active quiet window; `/clear <key>` confirms or lists open keys
- [ ] T017 [US1] fx wiring in cmd/main.go: `healthProviders()` gated by `BOT_HEALTH_CONFIG`; new env fields `BOT_HOST_NAME`, `BOT_SMS_PHONE`, SMTP vars; instance lock (second instance logs and runs without health); config entry errors reported as checker errors at start; the runner registered in the `worker` group; `/status` and `/clear` in `command_handler`; monitoring must start when Docker or Postgres are unavailable (FR-019)
- [ ] T018 [US1] CLI subcommands in cmd/cli.go with dispatch at the top of `main()` per contracts/cli.md: `check --once` (real adapters, printing notifier, store opened read-only if present, `<VERDICT> <key>: <text>` lines plus samples and a summary line, exit 0 ran / 1 setup error, sends and writes nothing) and `heartbeat --planned-reboot <seconds>` (0–86 400, 0 clears)
- [ ] T019 [US1] Documentation: README.md health-monitoring section (enabling, env vars from contracts/environment.md, commands, CLI, host requirements), generic `config/health.example.yaml` mirroring contracts/config.md, CLAUDE.md package map rows for `pkg/health/**`, `pkg/host/**`, `pkg/mail`

**Checkpoint**: US1 deployable on its own

---

## Phase 4: User Story 2 - Hardware health without configuration (Priority: P2)

**Goal**: Disks, temperatures and kernel errors, discovered automatically

**Independent Test**: spec US2 — recorded readings for healthy, degrading and failing hardware
produce the right alerts, baselines by serial, missing/retired messages

- [ ] T020 [P] [US2] Drive discovery and smartctl adapter in pkg/host/smartctl/smartctl.go (research R3, R4): list `/sys/block/*` keeping devices with a `device` link and skipping `loop*`, `ram*`, `zram*`, `dm-*`, `md*`, `sr*`, `removable=1`, `size=0`; run `smartctl --json=c -a /dev/<node>` with a timeout returning stdout and exit status; the sysfs filter is tested against a temp-dir fake tree
- [ ] T021 [US2] SMART JSON decoding in pkg/health/check/disks/smart.go with anonymised fixtures in pkg/health/check/disks/testdata/ (SATA healthy, SATA counters non-zero, NVMe healthy, NVMe critical warning and low spare, overall health failed, self-test failed, open failure exit bits 0–1): exit-status bitmask handling per R3; ATA attrs 5, 187, 188, 197, 198, 199 raw and NVMe `media_errors`, `num_err_log_entries`; "missing attributes treated as not reported"; temperature, percentage used, spare/threshold, data written, newest self-test result and power-on hours
- [ ] T022 [US2] Disks check in pkg/health/check/disks/disks.go: per-serial baselines (data-model "Baseline"); conditions `disks/<serial>/{health,nvme-warning,spare,wear,selftest,missing}`; one Event per counter rise; self-test failed or older than `selftest_max_age`; drive temperature judged here as condition `disks/<serial>/temp` against `temperatures.limits.drive_nvme` / `drive_sata` with the temperatures `threshold`; new drive announce and first-run single message listing all drives (FR-024); retired serial that reappears announced as new; `exclude_serials`; samples `drive.<serial>.wear`, `drive.<serial>.written_bytes`, `temp.drive.<serial>`
- [ ] T023 [P] [US2] hwmon and NVIDIA adapters in pkg/host/hwmon/hwmon.go and pkg/host/nvidia/nvidia.go (research R5): read `name`, `temp*_input`, `temp*_label`, `temp*_max`, `temp*_crit` from `/sys/class/hwmon/hwmon*` (fake tree in tests); run `nvidia-smi --query-gpu=index,name,temperature.gpu --format=csv,noheader,nounits` when present and parse its CSV (absent binary ⇒ no GPUs, not an error)
- [ ] T024 [US2] Temperatures check in pkg/health/check/temps/temps.go: hwmon and GPU only (drive temperatures are judged by the disks check, T022): classes `cpu` (coretemp/k10temp), `board` (other chips except `nvme`/`drivetemp`), `gpu`; per-class limits; one condition per sensor; a source missing from an otherwise good read is reported once as gone and its conditions cleared (FR-027); samples `temp.*`
- [ ] T025 [P] [US2] Kernel log and EDAC adapters in pkg/host/kmsg/kmsg.go and pkg/host/edac/edac.go (research R6): `/dev/kmsg` record parser `prio,seq,usec,flags;message` (tested on literal lines), a reader goroutine with context cancel, boot id from `/proc/sys/kernel/random/boot_id`; EDAC `mc*/ce_count`, `ue_count` (absent dir ⇒ no reading)
- [ ] T026 [US2] Kernel check in pkg/health/check/kernel/kernel.go: anchored pattern table per class (storage I/O, NVMe, ATA, machine check, thermal throttle); per class one message at start, at most one "still happening" per 6 h, quiet after 24 h without records; cursor `{BootID, Seq}` persisted in kv so restarts in the same boot neither repeat nor miss; EDAC counter rises as Events
- [ ] T027 [US2] `/retire` in pkg/command/command_retire.go plus runner support: match full serial or a unique suffix of ≥ 4 chars; clear all `disks/<serial>/*` conditions and mark the baseline retired with one "drive retired" message; ambiguous/unknown ⇒ list known drives
- [ ] T028 [US2] Wire hardware checks in cmd/main.go `healthProviders()` (adapters → check source interfaces) and document `smartmontools` as a host requirement in README.md

**Checkpoint**: US1 and US2 work independently

---

## Phase 5: User Story 3 - Service health from operator configuration (Priority: P3)

**Goal**: Containers, HTTP endpoints, freshness files and command probes with optional fixes

**Independent Test**: spec US3 — crash loop, endpoint outage, stale stamp, probe with fix action
and its rate limit, dependency skip

- [ ] T029 [P] [US3] Docker adapter in pkg/host/docker/docker.go: add `github.com/moby/moby/client`; `List(ctx)` returning `{Name, RestartPolicy, Running, Status, Health, RestartCount, StartedAt}` only (no labels, no command lines, FR-029) and `Restart(ctx, name)`
- [ ] T030 [US3] Containers check in pkg/health/check/containers/containers.go (research R7): watch restart policy `always`/`unless-stopped` minus `exclude` globs; Bad when not running, `restarting`, `unhealthy`, or `RestartCount`/`StartedAt` changed since a previous observation that saw it up; a container gone or no longer watched resolves once ("no longer exists" / "no longer watched"); Docker unavailable ⇒ Unknown
- [ ] T031 [P] [US3] DoH resolver in pkg/doh/doh.go (research R8): RFC 8484 POST `application/dns-message` via `golang.org/x/net/dns/dnsmessage` (promote to a direct dependency; approved by the maintainer), A then AAAA, honour TTL-free single use, timeout from the caller's context
- [ ] T032 [US3] HTTP check in pkg/health/check/httpcheck/httpcheck.go: status range, timeout, at most 5 redirects, `connect_to` or `doh` dialer keeping the URL host for TLS and Host, optional header whose value comes from the named env var (missing or placeholder-looking value ⇒ checker error, never Bad/Good, never logged), optional `container` health requirement, `depends_on` honoured by the runner
- [ ] T033 [P] [US3] Freshness check in pkg/health/check/freshness/freshness.go: `content: epoch | mtime`; Bad when older than `max_age`; a missing file is Bad only after `max_age` measured from the first run that saw it missing (first-missing time in kv); an unreadable file is Unknown
- [ ] T034 [US3] Command check in pkg/health/check/command/command.go: argv without shell, timeout, exit status judges; optional fix after `after` consecutive failures (restart container via the docker adapter or run argv), at most `max_per_hour`, not within `grace_after_start` of the target's start; every fix recorded as an alert of kind `fix`; `report: journal` keeps fixes out of Telegram
- [ ] T035 [US3] Wire service checks in cmd/main.go `healthProviders()`; a missing Docker socket leaves only the Docker-backed checks Unknown

**Checkpoint**: US1–US3 work independently

---

## Phase 6: User Story 4 - Incoming SMS and a proven modem (Priority: P4)

**Goal**: gammu inbox forwarding and the weekly modem round-trip probe

**Independent Test**: spec US4 — fake inbox with single/multipart messages and a probe reply

- [ ] T036 [P] [US4] gammu inbox repository in pkg/sms/gammu/inbox.go: select rows with `"Processed" = false` (`ID`, `SenderNumber`, `TextDecoded`, `UDH`, `ReceivingDateTime`), mark a set of IDs processed in one statement
- [ ] T037 [US4] Inbox worker in pkg/sms/inbox.go (research R14): every 15 s group parts by sender + UDH concatenation reference, forward when complete or after 10 minutes with the parts present, same Telegram format as the ZTE worker, then mark processed; a `ProbeMatcher` is consulted first and matched replies are not forwarded
- [ ] T038 [US4] Modem probe in pkg/health/check/modemprobe/modemprobe.go: weekly at `weekday`/`time`, random 12-hex token text, send via the SMS sender to the number in `phone_env`; match "sender compared by digits; by the last 9 digits when the two lengths differ" and exact text; PROBLEM at once on send failure, after `reply_within` (30 m) without reply, or when none returned for `max_silence` (8 d, counted from first run when never); RESOLVED by the next returned probe; state per data-model "ProbeState"
- [ ] T039 [US4] Wire the inbox worker and modem probe under `BOT_GAMMU_DSN` in cmd/main.go and document removing gammu's `runonreceive` hook in README.md

**Checkpoint**: US1–US4 work independently

---

## Phase 7: User Story 5 - History and reports over Telegram (Priority: P5)

**Goal**: `/history`, `/temp`, `/disks` and the weekly summary from stored history

**Independent Test**: spec US5 — seeded history spanning months; command replies, retention,
roll-up, summary content and length limit

- [ ] T040 [US5] History queries and retention in pkg/health/store/history.go: alerts by check/key prefix and period newest first; samples current + 7-day min/avg/max (raw and `samples_daily`); daily job per data-model "Retention" (alerts > 365 d deleted, samples > 90 d rolled up then deleted, queue > 7 d dropped and logged); runner daily hook
- [ ] T041 [P] [US5] `/history` in pkg/health/report/history.go and pkg/command/command_history.go per contracts/telegram-commands.md (args `[check] [period]`, default `7d`, `No alerts in <period>`)
- [ ] T042 [P] [US5] `/temp` and `/disks` in pkg/health/report/readings.go, pkg/command/command_temp.go and pkg/command/command_disks.go per contracts/telegram-commands.md (drives shown by bus, model and last 4 serial characters)
- [ ] T043 [US5] Weekly summary in pkg/health/report/summary.go plus the runner weekly hook: per-drive lines (health, wear, data written this week), services block, open problems; one message within the length limit cut with `…`; a section that cannot be built reads "unavailable"

**Checkpoint**: All user stories functional

---

## Phase 8: Polish & Cross-Cutting Concerns

- [ ] T044 Final documentation pass in README.md and CLAUDE.md (feature table, env table, package map, coverage-gate scope) and run every scenario of specs/001-host-health-monitoring/quickstart.md on a test host, recording any gaps as new tasks via `/speckit-converge`
- [ ] T045 Pre-push privacy sweep: review `git diff origin/main` and all `testdata/` fixtures for real host names, addresses, phone numbers, container names, drive serials or models and references to private repositories (patterns kept outside the repository); fix any hit before pushing (SC-009)

---

## Dependencies & Execution Order

- Phase 1 → Phase 2 → user-story phases. T002 is first in Phase 2 by decision (security gap).
- Within Phase 2: T003, T004, T005, T006, T008, T010, T011 are parallel; T007 needs T006;
  T009 needs T008 and T004; T012 needs T003–T009 and T011; T013 needs T012.
- User stories depend only on Phase 2; US2–US5 do not depend on each other except: T043 uses the
  disks data from US2 and services from US3 when present (sections read "unavailable"
  otherwise). T024 and T022 are independent: drive temperatures belong to the disks check.

## Parallel Examples

- Phase 2: T003, T004, T005, T006, T008, T010, T011 together.
- US1: T014 with T015.
- US2: T020, T023, T025 (adapters) together, then T021→T022, T024 and T026 in any order.
- US3: T029, T031, T033 together.
- US5: T041 with T042 after T040.

## Implementation Strategy

1. MVP = Phase 1 + Phase 2 + US1: deploy, run `tgshd check --once`, then retire the old
   network/UPS cron checks.
2. Add US2, then US3; after each, run the one-shot mode on the host and retire the matching old
   checks.
3. US4 requires the gammu hook change on the host; US5 adds value without replacing anything.
