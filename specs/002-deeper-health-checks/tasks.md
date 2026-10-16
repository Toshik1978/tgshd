---

description: "Task list for deeper health checks"
---

# Tasks: Deeper Health Checks

**Input**: Design documents from `specs/002-deeper-health-checks/`

**Prerequisites**: plan.md, spec.md, research.md (R1–R28), data-model.md, contracts/ (config.md,
telegram-commands.md, cli.md), quickstart.md, `.specify/memory/constitution.md`

**Tests**: There are no standalone test tasks. Every task is implemented test-first (standard
library `testing`, table-driven, hand-written fakes, anonymised fixtures under the package's
`testdata/`): its tests are written and seen to fail, then made to pass with `-race`. Each task
names the test file it extends or adds and, where one applies, the acceptance scenario
("US1 acc. 4"), the success criterion ("SC-002") and the quickstart test names it proves (SC-011).

**Organization**: No Setup phase is needed (no new dependency, no new tool, `COVERPKG` already
covers `./pkg/health/...`). Phase 2 holds the blocking prerequisites, Phases 3–8 follow the user
stories in priority order, Phase 9 is polish and the full gate. Each phase is one commit.

**Global rules for every task**

- No real host data in code, tests, fixtures or docs: serials `TEST0000000001`…, model
  `Example SSD`, hosts under `example.org`, addresses from RFC 5737, phone `+10000000000`;
  journal and kernel fixtures are synthetic lines.
- Standard library first; wrap errors with `%w`; follow the style of the file being edited; no
  new third-party dependency (plan, Principle III); `CGO_ENABLED=0`.
- Decisions stay pure in `pkg/health/**`; `pkg/host/**`, `pkg/doh` and `cmd` only do I/O and
  wiring. Adapters return raw data. Pure packages never import `pkg/host/**` or `pkg/doh`: an
  adapter's sentinel error is mapped onto the pure package's own sentinel by a cmd adapter.
- Every task leaves the build green: an interface, type or signature change, its adapter and all
  of its call sites (cmd included) are changed in the same task.
- Messages and logs never contain command output, header values or URLs.
- Every 001 config file keeps loading unchanged (contracts/config.md "Compatibility").
- Run `task lint` and `task test:coverage` before closing a task.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no unfinished prerequisite in the phase)
- **[Story]**: Which user story this task belongs to (US1–US6)
- A task without [P] depends on the task before it; extra prerequisites are written
  "(depends on Tnnn)".

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Per-distinct checker-error keys (needed by every checker error of US1–US6; the plan
moves FR-028 here) and the store schema v2 that the queue metadata of US2 and the one-shot CLI
rely on (R28)

**⚠️ CRITICAL**: No user story work can begin until this phase is complete

- [x] T001 Add `health.CheckError{Subject, Kind string; Err error}` with `Error()`, `Unwrap()` and a key helper in pkg/health/check.go (tests in pkg/health/check_test.go, new) (FR-028, R6)
    - Key: `<check>/<Subject>/<Kind>`, or `<check>/<Kind>` when Subject is empty (per-entry checks such as `http/<name>`, `freshness/<name>`, `command/<name>` whose name already names the subject)
    - Kind matches `[a-z0-9-]+` (e.g. `no-verdict`, `selftest-log`, `no-temperature`, `forced-read`, `edac-unreadable`, `lost`, `incomplete`, `auth`, `unreadable`); Subject is a drive serial, sensor key, controller, channel or a fixed component name (`discovery`, `kmsg`, `boottail`, `maintenance`, `cpu`), "never message text"
- [x] T002 Raise checker errors per distinct key in `Runner.apply` in pkg/health/runner/runner.go (tests in pkg/health/runner/runner_test.go: `TestDistinctCheckerErrors`, proves US2 acc. 6) (FR-028, R6)
    - Walk the joined error tree (`errors.As` and `Unwrap() []error` of `errors.Join`); each `CheckError` is raised under its key; any other error keeps the 001 key `<check>/run/error`
    - Rate limiting stays 24 h per key and channel through the existing `RateMarks`; a new key is delivered while another key of the same check is inside its window
- [x] T003 [P] Store schema migration 2 in `migrations()` in pkg/health/store/store.go and queue columns in pkg/health/store/state.go (tests in pkg/health/store/store_test.go: `TestMigrateV1ToCurrent`; pkg/health/store/state_test.go: `TestQueueKindRoundTrip`) (FR-030, FR-031 prerequisite, R28)
    - SQL verbatim: `ALTER TABLE queue ADD COLUMN kind TEXT NOT NULL DEFAULT 'event';` `ALTER TABLE queue ADD COLUMN check_name TEXT NOT NULL DEFAULT '';` `ALTER TABLE queue ADD COLUMN key TEXT NOT NULL DEFAULT '';` sets `user_version = 2` inside the existing migration transaction
    - "`store.validate` rejects a queue `kind` outside `problem`, `resolved`, `event`"; rows queued by 001 read back as `kind = event`
    - `OpenReadOnly` keeps requiring the current version and returns a distinguishable error for a version-1 database; a newer database still fails with `ErrSchemaTooNew`
- [x] T004 One-shot CLI: one line per distinct checker error and the schema-v1 fallback in cmd/cli.go (depends on T002, T003) (tests in cmd/cli_test.go) (FR-028, contracts/cli.md)
    - Prints `ERROR <check>/<subject>/<kind>: <text>` for every distinct checker error of a run
    - A database still at schema version 1 runs without stored state and prints `state database is at schema version 1; run the daemon once to migrate` (exit 0); a database newer than the build still exits 1

**Checkpoint**: Foundation ready; every check can return keyed checker errors

---

## Phase 3: User Story 1 - Drive failure is caught early and never hidden (Priority: P1) 🎯 MVP

**Goal**: The full built-in counter set, the NVMe spare floor, SATA SSD wear, calendar self-test
age with failures reported once, gap-aware reads that never resolve a problem, one forced read per
day of a sleeping drive, transport-filtered discovery and the drive status in `/disks`

**Independent Test**: Feed recorded drive reads (healthy; a rise in each default counter; spare at
19%; a read with no health verdict; a read missing its attribute table; a drive asleep for 25 h; a
drive whose last passed self-test is 41 days old) and verify the alerts, checker errors, kept state
and forced read (quickstart §1)

- [x] T005 [P] [US1] Disks config keys and the changed self-test default in pkg/health/config/types.go, pkg/health/config/defaults.go and pkg/health/config/validate.go (tests in pkg/health/config/config_test.go) (FR-001, FR-003, FR-006, FR-008, FR-011)
    - `disks.spare_floor_percent` int, default `20`, "0–100"
    - `disks.force_read_after` duration, default `24h`, "≥ `1h`"
    - `disks.include_serials` [string], default `[]`, "non-empty entries"
    - `disks.extra_counters` [int], default `[]`, "ATA ids 1–255"; a built-in id listed again is accepted and has no effect
    - `disks.extra_counters_by_serial` map serial → [int], default `{}`, "ids 1–255"; "serial keys non-empty"
    - Changed default: `disks.selftest_max_age` `8d` (power-on time) → `40d` (calendar time since a passed test was newly observed); an explicit `8d` is now calendar days, so it is the closest value to 001 but not an exact restore
- [x] T006 [P] [US1] Drive source shape: raw sysfs path and the wake flag through pkg/host/smartctl/smartctl.go, `disks.Source` in pkg/health/check/disks/disks.go and a `smartctlSource` adapter in cmd/health_hardware.go (tests in pkg/host/smartctl/smartctl_test.go: `TestDevicePath`, `TestWakeFlag`; pkg/health/check/disks/disks_test.go and pkg/health/check/disks/standby_test.go fakes updated; cmd/health_hardware_test.go: `TestSmartctlSourceMapsNodes`) (FR-001, FR-003, R4, R5)
    - `smartctl.Runner.Drives` returns `[]smartctl.Node{Name, Path}`: `Path` is the raw `filepath.EvalSymlinks("/sys/block/<node>")` result, not judged by the adapter; the 001 filters (`loop`, `ram`, `zram`, `dm-`, `md`, `sr`, `removable=1`, `size=0`) stay; tested against a temp-dir fake tree
    - `smartctl.Runner.Run(ctx, node, wake bool)`: `-n standby,3` when `wake` is false, `-n never` when true; with `wake=true` the adapter applies its own per-drive timeout `ForcedTimeout = 20 * time.Second` (a spinning-up disk needs it), so the pure disks package never refers to it
    - `disks.Source` becomes `Drives() ([]Node, error)` (`disks.Node{Name, Path string}`) and `Run(ctx, node string, wake bool)`; the existing call sites in `probe` and `read` use `Node.Name` and pass `wake=false`, so behaviour is unchanged
    - cmd: `disksChecks` passes `smartctlSource{runner: smartctl.NewDefault()}`, which converts `smartctl.Node` to `disks.Node` and forwards `wake`
- [x] T007 [US1] Decode the larger counter set, normalised values, `rotation_rate` and SATA SSD wear in `DecodeSMART` in pkg/health/check/disks/smart.go, pass the per-drive counter set and rename the counters in pkg/health/check/disks/disks.go, with fixtures `sata_counters_183.json`, `nvme_errlog_rise.json`, `sata_ssd_177.json` in pkg/health/check/disks/testdata/ (depends on T005) (tests in pkg/health/check/disks/smart_test.go; pkg/health/check/disks/disks_test.go: `TestBuiltinCounterRise` proves US1 acc. 1 and SC-001, `TestCounterNames`, `TestNVMeErrorLogSilent` US1 acc. 10) (FR-006, FR-007, FR-009, R1)
    - Table rows decode `value` (normalised, `*int`) next to `raw.value`; the root decodes `rotation_rate` (`*int`, `0` = solid state)
    - `DecodeSMART(stdout, exitStatus, counters)` takes the counter set; the only call site, `read` in disks.go, passes `counterSet(serial)` = "`built-in ∪ disks.extra_counters ∪ disks.extra_counters_by_serial[serial]`, ATA ids 1–255"; built-in ATA set {5, 179, 181, 182, 183, 187, 188, 195, 197, 198, 199} compared by `raw.value`
    - One Event per rise against the drive's own baseline `drive <serial> (<model>): <counter name> (ATA <id>) rose <a>→<b>`; a counter seen first only sets the baseline; `counterName` in disks.go drops the ids embedded in the 001 names (`reallocated sectors`, `reported uncorrectable errors`, `command timeouts`, `pending sectors`, `offline uncorrectable sectors`, `UDMA CRC errors`) and adds 179 `used reserved blocks`, 181 `program fail`, 182 `erase fail`, 183 `runtime bad blocks`, 195 `hardware ECC`; an operator id with no known name is `attribute <id>`; NVMe reads `media errors (NVMe)`
    - NVMe `media_errors` stays a counter; `num_err_log_entries` leaves `Report.Counters` and becomes the history-only sample `drive.<serial>.nvme_err_log`; `nvme.num_err_log_entries` is dropped from the baseline on the next write
    - SATA SSD wear: `Bus == ATA`, `rotation_rate == 0`, attribute 177 with normalised `value` in 0–100 ⇒ `Report.PercentageUsed = 100 - value` clamped to 0–100
- [x] T008 [US1] Tri-state health verdict, section gaps, self-test classification and the unsupported-format flag in pkg/health/check/disks/smart.go, and the no-verdict judgement in pkg/health/check/disks/disks.go, with fixtures `no_smart_status.json`, `no_attribute_table.json`, `no_selftest_log.json`, `selftest_unsupported.json`, `sas_selftest.json` in pkg/health/check/disks/testdata/ (tests in pkg/health/check/disks/smart_test.go: `TestHealthVerdict`, `TestSectionGaps`, `TestSelfTestClassification`, `TestSCSISelfTestUnsupported`; pkg/health/check/disks/disks_test.go: `TestNoVerdictKeepsProblem` proves US1 acc. 2 and SC-002) (FR-004, FR-005, FR-010, FR-012, FR-013, R2, R3)
    - Health is explicit only when `smart_status.passed` is present or exit status bit 3 is set; otherwise `unknown` (`HealthFailed bool` becomes a tri-state `Health`); its call site in `judgeDrive` changes in this task: no verdict ⇒ no `disks/<serial>/health` reading (an open problem stays open, no RESOLVED) and `CheckError` `disks/<serial>/no-verdict`; the baseline `data` JSON gains `health` (`ok` | `failing` | `unknown`, the last read's verdict) for `/disks`
    - `Gaps`: ATA without `ata_smart_attributes.table` ⇒ `attributes`; NVMe without `nvme_smart_health_information_log` ⇒ `nvme-health-log`; ATA or NVMe without `ata_smart_self_test_log` / `nvme_self_test_log` ⇒ `selftest-log`; SCSI/SAS needs only the verdict; no `temperature.current` and no NVMe log `temperature` ⇒ no temperature
    - Classification: ATA status nibble (`status.value >> 4`) 0 passed, 1–2 aborted, 15 in progress, every other code failed; NVMe `self_test_result.value` 0 passed, 1–3 aborted, 0xF unused entry (skipped), every other code failed
    - Entries carry identity `SelfTestID{hours int64, type int, code int}`, newest first; a log present but not decodable (no `status.value` / `self_test_result.value`, an `extended`-only ATA log) sets `SelfTestFormat = unsupported`; a SCSI/SAS drive is always `unsupported`, whether or not it has a log, and never raises `selftest-log` or any other checker error
- [x] T009 [US1] Self-test head alignment, new failures and calendar age as pure functions in pkg/health/check/disks/selftest.go (tests in pkg/health/check/disks/selftest_test.go, new) (FR-010, FR-011, R3)
    - Head = identities of the newest ≤ 5 entries seen last time; entries above the first position where the head matches are new; when the head cannot be aligned only entries newer than the stored newest power-on hours are new; a first sighting (or an empty 001 head) takes the whole log as the head with no Event
    - A newly observed `passed` entry sets `selftest_ok_at = now`; "`selftest_ok_at` ≤ now (a future value from a clock step is clamped to now)"; age = `now - max(selftest_ok_at, first_seen_at)`
    - Returns the new failed entries that are not the newest finished entry (each one Event), the new head and `selftest_newest {result, description}`
- [x] T010 [US1] Judge the spare floor, wear, missing sections and missing temperature in pkg/health/check/disks/disks.go with the fixture `nvme_spare_19.json` (threshold 10, spare 19) in pkg/health/check/disks/testdata/ (tests in pkg/health/check/disks/disks_test.go: `TestSpareFloor` proves US1 acc. 3, `TestSpareAtDriveThreshold`, `TestMissingSection` SC-002, `TestSATAWear`, `TestNoTemperature`) (FR-005, FR-008, FR-009, FR-013)
    - Spare Bad when `available_spare <= available_spare_threshold` or `available_spare < spare_floor_percent`; naming rule: at or below the drive's own threshold the text is `available spare <n>% at or below its threshold <t>%`, otherwise `available spare <n>% below the <f>% floor`; a drive with a spare and no threshold is judged against the floor alone
    - A gap ⇒ `CheckError` `disks/<serial>/<section>` and the conditions that section feeds keep their state (`judgeCounters` is skipped, so counters are not seen as gone); no temperature ⇒ `disks/<serial>/no-temperature`
    - SATA SSD wear flows through the existing wear path (`aspectWear`, `drive.<serial>.wear`) against `wear_percent`
- [x] T011 [US1] Judge self-tests by calendar age and report new failures once in pkg/health/check/disks/disks.go (tests in pkg/health/check/disks/disks_test.go: `TestSelfTestCalendarAge` proves US1 acc. 5, `TestSelfTestGrace` US1 acc. 6, `TestFailureThenPass` US1 acc. 7, `TestUnsupportedSelfTestLog`, `TestSelfTestLogMissingKeepsState` SC-002) (FR-010, FR-011, FR-012, R3)
    - Baseline `data` JSON gains `selftest_head`, `selftest_newest`, `selftest_format` (`ok` | `unsupported`); "Unknown fields from a newer build are ignored; missing fields read as zero values"
    - `disks/<serial>/selftest` Bad when the newest finished test failed or the age exceeds `selftest_max_age`, text `last passed self-test <n> d ago, limit <m> d`; a new failure that is not the newest finished entry is one Event `disks/<serial>/selftest-failure` `self-test failed (<description>) at <h> h; a newer test has passed since`
    - Format `unsupported` (always for SCSI/SAS) ⇒ no reading and no checker error; log section missing ⇒ no reading and the self-test baseline fields stay; sample `drive.<serial>.selftest_age_h` carries calendar hours
- [x] T012 [US1] Sleeping drives: asleep and unreadable since, and forced reads limited by `forced_read_at` and to one per run, in pkg/health/check/disks/disks.go (tests in pkg/health/check/disks/standby_test.go: `TestForcedRead` proves US1 acc. 4 and SC-003, `TestForcedReadNotRetried` SC-003, `TestOneForcedReadPerRun`, `TestForcedReadFailureKeepsProblems` SC-002) (FR-003, R4)
    - Baseline gains `asleep_since` ("First run that found the drive asleep since its last read; 0 when awake"), `unreadable_since` ("First failed read (or forced read) since the last successful read; 0 otherwise") and `forced_read_at` ("Last forced read attempt, successful or not; 0 when never")
    - A drive found asleep (exit 3, attributed by `attribute`) is due when `now - max(last_seen_at, forced_read_at) >= force_read_after`, so a forced read, successful or failed, is not retried before `force_read_after` has passed again (`TestForcedReadNotRetried`: a failed forced read, then runs at +1 h and +23 h do not force, +24 h does)
    - At most one forced read per run, after the routine reads, serialised, on the due drive with the oldest `max(last_seen_at, forced_read_at)`; the other due drives wait for later runs (`TestOneForcedReadPerRun`: three due drives are force-read in three runs, oldest first); `forced_read_at = now` is set before the result is judged; `wake=true` (the adapter applies its 20 s forced-read timeout), bounded by the run deadline through `health.Bound`
    - A failed forced read ⇒ `CheckError` `disks/<serial>/forced-read` (`drive asleep since <time> could not be read when woken: <error>`), `unreadable_since` set, every open problem kept
    - An orphan sleeping node is due after the same age (in-memory asleep-since and forced-read times) and takes the same single slot, which identifies it; the in-memory `asleepOrphanLimit` is removed
- [x] T013 [US1] Transport classification, discovery filter, `include_serials`, empty discovery and the upgrade retire in pkg/health/check/disks/transport.go and pkg/health/check/disks/disks.go, with a logger option wired in cmd/health_hardware.go (tests in pkg/health/check/disks/transport_test.go, new: `TestClassifyTransport`; pkg/health/check/disks/disks_test.go: `TestUSBSkipped` proves US1 acc. 8, `TestEmptyDiscovery` US1 acc. 9, `TestUpgradeUSBNotMissing`; cmd/health_hardware_test.go) (FR-001, FR-002, R5)
    - Pure `ClassifyTransport(path string) string`: a path segment `usb` ⇒ `usb`; `nvme` ⇒ `nvme`; `ata` ⇒ `sata`; `end_device-`, `sas_` or `port-` under a SCSI host ⇒ `sas`; anything else ⇒ `other`; tested on literal paths
    - `nvme`, `sata`, `sas` nodes are read; `usb` and `other` nodes are read only when `include_serials` is non-empty and a drive read from them is tracked only when its serial is listed (otherwise discarded: never tracked, never missing); baseline gains `transport` (`nvme`, `sata`, `sas`, `usb`, `other`)
    - No node at all while at least one active, non-excluded drive is known ⇒ `CheckError` `disks/discovery/empty` (`no drives found while <n> are known; missing-drive check suspended`) and no readings, so no "drive missing" opens
    - Upgrade: a stored baseline whose drive now classifies as `usb` or `other` (its stored `transport`, or for a 001 baseline the class of its stored node when discovered) and whose serial is not in `include_serials` is retired silently with one log line `drive <serial> on a <transport> transport is no longer tracked; list it in include_serials to keep watching it`; it never opens "drive missing" (`TestUpgradeUSBNotMissing`)
    - `disks.New` gains a variadic `WithLogger(*zap.Logger)` option (default no-op); `disksChecks` passes the fx logger
- [x] T014 [US1] Export drive status in pkg/health/check/disks/status.go (tests in pkg/health/check/disks/status_test.go, new) (FR-003, FR-009, FR-012)
    - `disks.Check.Status() []DriveStatus` from baselines and the last run: Serial, Model, Bus, Transport, Node, Health (baseline `health`), LastReadAt (`last_seen_at`), AsleepSince, UnreadableSince, Temperature (last `temp.drive.<serial>`), SelfTestNewest, SelfTestPassAge (`now - max(selftest_ok_at, first_seen_at)`), SelfTestReadable (`selftest_format == ok`), Wear, Spare, SpareThreshold; Mounts left for the report layer
- [x] T015 [US1] `/disks` line with wear, spare floor, self-test and last read in pkg/health/report/readings.go and cmd/health_reports.go (tests in pkg/health/report/readings_test.go and pkg/command/command_readings_test.go) (FR-003, FR-009, FR-012, contracts/telegram-commands.md `/disks`)
    - `health` comes from `DriveStatus.Health` (`unknown` after a read without a verdict) and `missing` from the open missing condition, instead of from whether the health condition is open
    - Line: `<node> <bus> <model> …<last 4 of serial>: health <OK|FAILING|unknown|missing>, wear <n>% (7d a/b/c), spare <n>% (floor <f>%, drive <t>%), temp <n>°C (7d a/b/c), written 7d <size>, self-test <result> <age>, last read <age> ago[, asleep since <time>][, unreadable since <time>]`
    - `wear` covers NVMe percentage used and SATA SSD wear; `spare` only for NVMe; self-test reads `passed 12 d ago`, `failed 3 d ago`, `none (first seen 5 d ago)` or `not readable` (always for SAS/SCSI); a drive read in the last run shows `last read <1 m ago`
- [x] T016 [US1] One-shot CLI never wakes a drive and prints sleeping drives, in cmd/health_hardware.go and cmd/cli.go (tests in cmd/health_hardware_test.go and cmd/cli_test.go: `TestOneShotAsleep`) (FR-003, contracts/cli.md)
    - Under `check --once` the disks check is built with a `WithoutForcedRead()` option, so no run uses `wake=true`
    - A drive in standby prints `ASLEEP disks/<serial>: last read <age> ago`

**Checkpoint**: US1 deployable on its own: `go test ./pkg/health/check/disks/ ./pkg/host/smartctl/` passes the quickstart §1 list

---

## Phase 4: User Story 2 - Problems are delivered and resolved correctly (Priority: P2)

**Goal**: Telegram failure tracking with one email, the SMS fallback with RESOLVED by SMS for
PROBLEMs that went by SMS, re-raising or listing of dropped PROBLEMs (never dropped to a log line),
the store recovery notice, hardware checks that run through quiet windows unless set to pause, and
the stuck-lock limit

**Independent Test**: With fake Telegram, SMS and email channels, make Telegram fail for 20 minutes
while a PROBLEM is raised, drop a PROBLEM from the retry queue, hold the maintenance lock for 7 h
while a drive counter rises, and raise two different checker errors from one check (quickstart §2)

- [x] T017 [P] [US2] Add `Engine.Reopen(key)` in pkg/health/engine.go (tests in pkg/health/engine_test.go: `TestReopen`) (FR-031, R19)
    - "`Open = false`, `OpenedAt` cleared, `StreakStart` and `BadFor` kept"; the next Bad reading raises the PROBLEM again at once; a Good reading ⇒ Healthy with no RESOLVED
- [x] T018 [P] [US2] Quiet-window and delivery config keys in pkg/health/config/types.go, pkg/health/config/checks.go, pkg/health/config/defaults.go and pkg/health/config/validate.go, and `Check.PauseInQuiet` in pkg/health/check.go (tests in pkg/health/config/config_test.go) (FR-026, FR-027, FR-029, R15)
    - Common key `pause_in_quiet_windows` bool on every check: default `false` for `disks`, `temperatures`, `kernel`; `true` for every other check (resolved per section)
    - `general.maintenance_max_hold` duration, default `6h`, "> 0"
    - `general.telegram_failing_after` duration, default `15m`, "≥ `1m`"
- [x] T019 [P] [US2] Carry `Kind`, `Check` and `Key` on queued messages in pkg/health/notify/notify.go and map them to the queue columns in pkg/health/runner/persist.go (tests in pkg/health/notify/notify_test.go and pkg/health/runner/persist_test.go) (FR-030, FR-031, R18, R28)
    - `QueuedMessage{ID, CreatedAt, Channel, Text, Attempts, Kind, Check, Key}`; "`Kind` ∈ `problem`, `resolved`, `event`; rows from 001 read as `event`"
- [x] T020 [US2] Pure channel state, SMS-sent set and undelivered list with kv codecs in pkg/health/notify/channels.go (tests in pkg/health/notify/channels_test.go, new: `TestChannelHealthTransitions`, `TestUndeliveredKeptUntilShown`, `TestUndeliveredLinesCarryOver`, `TestUndeliveredOverflowCounted`) (FR-029, FR-030, FR-031, SC-005, R17, R18, R19)
    - `ChannelHealth` per channel `{FailingSince time?, Reported bool}` (kv `notify.channels`): OK ⇒ Failing(since) on a failed send or a message queued while offline; Failing ⇒ Reported after the notice; any successful send ⇒ OK
    - SMS-sent set of condition keys (kv `notify.sms_problems`)
    - Undelivered list (kv `notify.undelivered`) `{Items []{Key, Text, Since, DroppedAt}, Folded int}`: `Items` "at most 50", oldest first; a PROBLEM added while `Items` is full increments `Folded`; nothing is ever dropped to a log line only
    - `Prefix(limit)` returns `Undelivered earlier:`, then as many `PROBLEM: <text> (since …), no longer open` lines as fit the limit, then `… and N more undelivered PROBLEMs` when `Folded > 0`; that counted line is always shown (its space is reserved before the item lines); `Ack(shown)` removes only the items that prefix showed and resets `Folded`, so the lines that did not fit stay for the next message
- [x] T021 [US2] Telegram failure tracking and the one email-only notice in pkg/health/notify/notify.go, pkg/health/notify/checker.go and pkg/health/runner/deliver.go (depends on T018) (tests in pkg/health/notify/notify_test.go and pkg/health/notify/checker_test.go: `TestTelegramFailingEmailOnce` proves US2 acc. 1 and SC-004) (FR-029, R17)
    - A failed Telegram send (any error other than `ErrPastDeadline`) or a Telegram message queued while offline sets `FailingSince` if empty; any successful Telegram send clears it and `Reported`
    - After each tick's deliveries, when `now - FailingSince >= telegram_failing_after` and not `Reported`: one email through `Mailer` only (never Telegram), key `notify/telegram/failing`, text `Telegram delivery failing since <time>`; without a mailer the text is logged at error level; then `Reported = true`
- [x] T022 [US2] SMS fallback for PROBLEMs and their RESOLVED in pkg/health/notify/notify.go (tests in pkg/health/notify/notify_test.go: `TestSMSFallbackProblem`, `TestResolvedBySMSWhenProblemWasSMS`, prove US2 acc. 2 and SC-004) (FR-030, R18)
    - While `FailingSince` is set, each queued PROBLEM (also PROBLEMs already queued when the failure starts) is sent by SMS once in plain form and its key enters the SMS-sent set; the Telegram copy stays queued
    - A RESOLVED whose key is in the set also goes by SMS and the key leaves the set; events and other RESOLVED wait for Telegram
    - Checks whose own mode is `sms` or `tlg-or-sms` keep their 001 routing (never a second SMS); without an SMS sender PROBLEMs stay queued
- [x] T023 [US2] Dropped PROBLEMs: reopen, or list until shown, in pkg/health/notify/notify.go and pkg/health/runner/runner.go (depends on T017) (tests in pkg/health/runner/runner_test.go: `TestDroppedProblemReopens`, `TestDroppedProblemNewStreakListed`, `TestDroppedResolvedListed`, prove US2 acc. 3 and SC-005; pkg/health/notify/notify_test.go: `TestUndeliveredPrefix`) (FR-031, R19)
    - `OnDrop` (queue trimmed at `MaxQueue` or expired after 7 days) hands the runner the item with `Kind` and `Key`; for a dropped PROBLEM, three branches, each tested: condition still open ⇒ `Engine.Reopen(key)`; condition tracked but not open (a new streak) ⇒ added to the undelivered list; condition no longer tracked ⇒ added to the undelivered list
    - The next message delivered to Telegram carries `Prefix(limit)` ahead of its own text, within the 4 096-unit limit; after that delivery succeeds `Ack` removes the items it showed; items that did not fit, and the counted line, go with the next delivered message
- [x] T024 [US2] Persist channel health, the SMS-sent set and the undelivered list in kv in pkg/health/runner/persist.go (tests in pkg/health/runner/persist_test.go) (FR-029, FR-030, FR-031)
    - `KVPersister`s for `notify.channels`, `notify.sms_problems`, `notify.undelivered`; a restart neither sends the Telegram-failing email twice, nor loses the SMS-sent set, nor loses a listed PROBLEM or the `Folded` count
- [x] T025 [US2] State-write recovery notice in `Runner.checkpoint` in pkg/health/runner/runner.go (tests in pkg/health/runner/runner_test.go: `TestStoreRecoveryNotice` proves US2 acc. 7) (FR-032, R20)
    - `saveReported` in memory; the first successful checkpoint after a reported `health/store/write` pushes one `<host>: CHECKER OK: health: state can be saved again` (Telegram, and email when the failure went by email), clears `saveReported` and reverts the `health/store/write` rate marks
- [x] T026 [US2] Quiet ticks run the checks that are not set to pause in pkg/health/runner/runner.go (depends on T018) (tests in pkg/health/runner/runner_test.go: `TestHardwareRunsInMaintenance` proves US2 acc. 4 and SC-006, `TestBootGraceHardware`) (FR-026, R15)
    - In `Runner.Do` a quiet tick (maintenance or boot grace) runs the due checks with `PauseInQuiet == false` through `runChecks`, then raises, flushes, dispatches and checkpoints as a normal tick; checks with `PauseInQuiet == true` (containers, network by default) are not judged; the kmsg reader always runs
- [x] T027 [US2] Maintenance lock observation and the stuck limit in pkg/health/runner/maintenance.go, `Snapshot.Paused` in pkg/health/runner/runner.go and the configuration-aware quiet note in pkg/health/report/status.go (tests in pkg/health/runner/maintenance_test.go, new: `TestStuckMaintenanceLock` proves US2 acc. 5; pkg/health/runner/runner_test.go: `TestSnapshotPaused`; pkg/health/report/status_test.go: `TestQuietNoteListsPaused`) (FR-026, FR-027, R15, R16)
    - `{HeldSince time?, LastSeenHeld time?}` in kv `health.maintenance`: a tick finding the lock held sets `HeldSince` if empty and updates `LastSeenHeld`; a free lock clears both; "Continuity across restarts only when `now - LastSeenHeld ≤ 5 min`"
    - `now - HeldSince > general.maintenance_max_hold` ⇒ `CheckError` `health/maintenance/stuck` (`maintenance lock held since <time> (more than <max>)`); the window itself stays
    - `Snapshot` gains `Paused []string` (the configured checks with `PauseInQuiet`); `/status` quiet note: `Note: quiet window active (maintenance, held since <time>); paused: <check>, …` / `Note: quiet window active (boot grace); paused: <check>, …`, or `paused: none` when no check is set to pause
- [x] T028 [US2] Wire `PauseInQuiet` per check, `maintenance_max_hold`, `telegram_failing_after`, the SMS fallback sender and the mailer in cmd/health.go (tests in cmd/health_test.go) (FR-026, FR-027, FR-029, FR-030)
    - The daemon still starts without SMS, SMTP or a store (Principle V): no SMS ⇒ PROBLEMs stay queued; no SMTP ⇒ the Telegram failure is logged

**Checkpoint**: US1 and US2 work independently; quickstart §2 passes

---

## Phase 5: User Story 3 - Complete kernel and memory error coverage (Priority: P3)

**Goal**: The previous boot's journal tail, the wider class table, lost-record counts, "still
happening" without a new record, every uncorrectable memory rise and per-controller EDAC reads

**Independent Test**: Feed recorded kernel records of each class and a previous-boot journal tail;
feed memory-controller counters with an uncorrectable rise and one unreadable controller; verify
the messages and checker errors (quickstart §3)

- [x] T029 [P] [US3] Journal adapter in pkg/host/journal/journal.go with the synthetic fixture pkg/host/journal/testdata/journal_prev_boot.jsonl (tests in pkg/host/journal/journal_test.go, new: `TestDecodeJSONLines`, `TestUnavailable`, `TestRecordCap`) (FR-020, R7)
    - Runs `journalctl --boot=<boot id> --dmesg --output=json --output-fields=MESSAGE,PRIORITY,SYSLOG_FACILITY,_SOURCE_MONOTONIC_TIMESTAMP --no-pager --quiet` and streams stdout through `json.Decoder`, one object per line; `MESSAGE` decodes as a string or an array of numbers
    - Honours its context (no timer of its own; the check bounds it); stops after at most 200 000 records with `ErrCapped`, having handed over the records read
    - `ErrUnavailable` when the binary is absent or `journalctl` reports "No journal files were found" or "Data from the specified boot … is not available"
- [x] T030 [P] [US3] Per-controller EDAC errors through pkg/host/edac/edac.go, `kernel.Counters` in pkg/health/check/kernel/kernel.go and the `edacSource` conversion in cmd/health_hardware.go (tests in pkg/host/edac/edac_test.go: `TestPerControllerErr`; cmd/health_hardware_test.go) (FR-025, R11)
    - `edac.Reader.Read` returns every controller, each with its own `Err` (counter file missing or unparsable) instead of failing the whole read
    - `kernel.Counters` gains `Err error` (`json:"-"`), so `kernel.Counters(in[i])` in cmd keeps compiling; until T034 a controller with `Err` keeps its stored values and is not judged
- [x] T031 [US3] Ordered class table at any level in `classTable()` in pkg/health/check/kernel/kernel.go (tests in pkg/health/check/kernel/kernel_test.go: `TestGHESRecord`, `TestIOErrorAnywhere`, `TestATALinkResetAnyLevel`, prove US3 acc. 2) (FR-021, R8)
    - Specific to general, first match wins, every class accepts any level
    - `kernel/machine-check`: `\[Hardware Error\]` anywhere; `Machine check events logged`; `^mce: ` lines with `Machine Check`
    - `kernel/ata`: `^ata\d+(\.\d+)?: SError: `; `^ata\d+(\.\d+)?: (hard |soft )?resetting link`; `^ata\d+: (COMRESET failed|link is slow to respond|limiting SATA link speed)`; 001's `exception|failed command|hard resetting`; `SATA link down` / `SATA link up` are not matched
    - `kernel/storage-io` last: `I/O error` anywhere, excluding `dev (loop|sr|fd|zram)\d`
- [x] T032 [US3] Count lost kernel records in `Check.Consume` and `Run` in pkg/health/check/kernel/kernel.go (tests in pkg/health/check/kernel/kernel_test.go: `TestLostRecords` proves US3 acc. 6) (FR-023, R9)
    - `Seq > maxSeq + 1` ⇒ `Seq - maxSeq - 1` lost; the first record of the current boot is compared with the stored cursor; matching records dropped because the inbox was full (`maxInbox`) count too
    - One `CheckError` `kernel/kmsg/lost` per run with `lost > 0`: `<n> kernel records were lost before they could be read`
- [x] T033 [US3] "Still happening" without a new record in pkg/health/check/kernel/kernel.go (tests in pkg/health/check/kernel/kernel_test.go: `TestStillHappeningWithoutNewRecord` proves US3 acc. 3) (FR-022, R10)
    - `Episode` gains `Unreported int` (kv `episodes.*.unreported`); `Run` walks every episode and sends `Kernel: <class> still happening (<n> records since <time>). Last: <line>` when `Unreported > 0` and 6 h passed since the last message, then clears `Unreported`
- [x] T034 [US3] Every uncorrectable rise at once and unreadable controllers in pkg/health/check/kernel/kernel.go (tests in pkg/health/check/kernel/edac_test.go: `TestUncorrectableEveryRise` proves US3 acc. 4, `TestUnreadableController` US3 acc. 5) (FR-024, FR-025, R10, R11)
    - `ue` rises bypass episodes: each is one Event `Kernel: uncorrectable memory errors on <mc> rose <a>→<b>`; `ce` rises keep the 001 episode throttling
    - "an `Err` controller keeps its stored values and raises `kernel/<controller>/edac-unreadable`" (`memory controller <mc> counters unreadable: <error>`); the other controllers are judged
- [x] T035 [US3] Previous boot's tail at start, bounded, in pkg/health/check/kernel/boottail.go and pkg/health/check/kernel/kernel.go (tests in pkg/health/check/kernel/boottail_test.go, new, and pkg/health/check/kernel/kernel_test.go: `TestPreviousBootTail` proves US3 acc. 1 and SC-007, `TestNoPersistentJournal`, `TestBootTailIncomplete`) (FR-020, R7)
    - Cursor gains `usec` ("kmsg time since boot of the record at `seq`; matches the journal's `_SOURCE_MONOTONIC_TIMESTAMP`"); a `JournalSource` interface, the sentinels `kernel.ErrNoJournal` and `kernel.ErrJournalCapped` and the variadic options `WithJournal(JournalSource)` and `WithLogger(*zap.Logger)` (no change to `kernel.New`'s parameters); the tail is every record of the stored boot with a timestamp greater than `usec`, ties compared by message
    - At the first `Run` with a stored `boot_id` ≠ current: read the tail before resetting the cursor, bounded by `health.Bound` at `health.MaxCheckTimeout` (20 s); classify it with the class table, one Event per class `Kernel (previous boot): <class> found (<n> records). Last: <line>`; only then cursor := `{current boot, 0, false, 0}`
    - Incomplete (the deadline cut the read, or the source returned `ErrJournalCapped`): the records read are still reported, `CheckError` `kernel/boottail/incomplete` (`previous boot's kernel log tail was not read completely: <time limit | record cap>`), and the cursor moves to the new boot after that one attempt, so the tail is not retried (`TestBootTailIncomplete` covers the timeout and the cap)
    - `ErrNoJournal` ⇒ one log line `previous boot's kernel log tail could not be checked: no persistent journal`, no checker error, cursor reset
- [x] T036 [US3] Wire the journal source in cmd/health_hardware.go (depends on T029, T035) (tests in cmd/health_hardware_test.go: `TestJournalSourceMapsErrors`) (FR-020, contracts/cli.md)
    - A `journalSource` adapter over `pkg/host/journal` maps `journal.ErrUnavailable` to `kernel.ErrNoJournal` and `journal.ErrCapped` to `kernel.ErrJournalCapped`; the daemon's kernel check gets `WithJournal` and `WithLogger`; it starts without `journalctl` (Docker image)
    - `check --once` builds the kernel check without `WithJournal`, so it does not read the previous boot's tail

**Checkpoint**: US1–US3 work independently; quickstart §3 passes

---

## Phase 6: User Story 4 - Quieter, accurate temperature alerting (Priority: P4)

**Goal**: New default limits judged at or above, per-serial drive limits, one sensor per CPU
package, listed board sensors only, a plausible range, unreadable never treated as gone, and
"gone" only for open problems

**Independent Test**: Feed recorded sensor readings: per-core and per-package CPU values, an
unlisted board sensor with and without a critical limit, a drive at exactly 60 °C, a sensor that
stops reading while its problem is open, a GPU that stops reporting, and a sensor that disappears
without an open problem (quickstart §4)

- [x] T037 [P] [US4] Temperature defaults and new keys in pkg/health/config/types.go, pkg/health/config/defaults.go and pkg/health/config/validate.go (tests in pkg/health/config/config_test.go) (FR-014, FR-015, FR-017, R14)
    - Changed defaults: `temperatures.threshold` `15m` → `0`; `limits.cpu` `85` → `90` (package sensor); `limits.drive_nvme` `70` → `60`; `limits.drive_sata` `55` → `60` (also SAS); `limits.gpu` `85` and `limits.board` `75` keep their values
    - `temperatures.limits_by_serial` map serial → int °C, default `{}`, "0–150"
    - `temperatures.board_sensors` list of `{chip, label, limit}`, default `[]`: "`chip` required; `label` optional (all labels); `*` glob in both; `limit` 0–150, default `limits.board`"
- [x] T038 [P] [US4] Sensor device path and unreadable inputs through pkg/host/hwmon/hwmon.go, `temps.Sensor` in pkg/health/check/temps/temps.go and the `hwmonSource` conversion in cmd/health_hardware.go (tests in pkg/host/hwmon/hwmon_test.go: `TestUnreadableInput`, `TestSensorDevice`; pkg/health/check/temps/temps_test.go; cmd/health_hardware_test.go) (FR-016, FR-018, R12, R13)
    - `hwmon.Sensor` and `temps.Sensor` both become `{Chip, Label, Device string; Celsius float64; Max, Crit *float64; Err error}`, so `temps.Sensor(in[i])` in cmd keeps compiling; `Device` is the resolved hwmon device path; a `temp<n>_input` that exists but cannot be read (`EIO`, `ENODATA`, garbage) is kept with `Err` set
    - Until T044 the check skips a sensor with `Err` (as 001 skipped the unreadable input), so no reading of 0 °C is ever judged
- [x] T039 [US4] No-devices sentinel and `[N/A]` GPUs through pkg/host/nvidia/nvidia.go, `temps.GPU` in pkg/health/check/temps/temps.go and the `nvidiaSource` adapter in cmd/health_hardware.go (tests in pkg/host/nvidia/nvidia_test.go: `TestNoDevicesSentinel`, `TestNAKept`; cmd/health_hardware_test.go: `TestNvidiaSourceMapsNoDevices`) (FR-018, R13)
    - `nvidia.ErrNoDevices` for "No devices were found"; `Parse` keeps a GPU whose temperature is `[N/A]` with `Celsius == nil`; `nvidia.GPU.Celsius` and `temps.GPU.Celsius` both become `*float64`; an absent `nvidia-smi` still means no GPUs
    - cmd maps `nvidia.ErrNoDevices` to the new `temps.ErrNoGPUs`; until T044 the check treats `ErrNoGPUs` as no GPUs and skips a nil temperature (001 behaviour)
- [x] T040 [US4] Drive temperatures at or above the limit with per-serial overrides in `judgeTemp` in pkg/health/check/disks/disks.go (depends on T037) (tests in pkg/health/check/disks/disks_test.go: `TestDriveTempAtLimit` proves US4 acc. 1, `TestDriveTempBySerial` US4 acc. 7) (FR-014, FR-015, R14)
    - `limits_by_serial[serial]` first, else `drive_nvme` / `drive_sata` (SAS uses `drive_sata`); broken when the reading is at or above the limit; threshold 0 ⇒ the first hot reading is Bad (the `overFor` streak path stays for a non-zero threshold)
- [x] T041 [US4] One sensor per CPU package and the no-CPU checker error in pkg/health/check/temps/temps.go (depends on T037, T038) (tests in pkg/health/check/temps/temps_test.go: `TestPackageOnly` proves US4 acc. 2, `TestNoCPUSensor`) (FR-016, R12)
    - `coretemp`: only `Package id <n>`, condition `temps/coretemp/package-id-<n>` (the 001 key); `Core <n>` neither judged nor sampled
    - `k10temp` / `zenpower`: one condition per chip instance from `Tdie`, else `Tctl`; `Tccd<n>` ignored; instances ordered by `Sensor.Device` so the second package keeps the `-2` suffix
    - A formerly judged core sensor is listed in `Forget` once: silently without an open problem; with one, an Event `<sensor> is no longer judged separately; the package sensor covers it`
    - Check enabled and a complete hwmon read with no CPU package sensor ⇒ `CheckError` `temps/cpu/none`
- [x] T042 [US4] Board allow-list, own critical limit, plausible range and at-or-above in pkg/health/check/temps/temps.go, with the temperatures section reaching the check through `buildTempsCheck` in cmd/health_hardware.go (tests in pkg/health/check/temps/temps_test.go: `TestUnlistedBoardNoCrit` proves US4 acc. 3 and SC-009, `TestBoardCrit`, `TestImplausible`, `TestGPUAtLimit`) (FR-014, FR-017, R13)
    - `temps.New(hwmon, nvidia, cfg config.Temperatures)` takes the whole section instead of `config.Limits`, so `board_sensors` reaches the check; its call site in `buildTempsCheck` changes in this task
    - Board sensors (any chip other than CPU, `nvme`, `drivetemp`) judged only when listed in `board_sensors` (chip and label globs, entry `limit` or `limits.board`), or unlisted against their own `temp<n>_crit`; unlisted without crit ⇒ sample only
    - "Plausible range −40…150 °C; outside ⇒ Unknown" (a checker error, never Bad); `apply` compares at or above for every class, GPUs included
- [x] T043 [US4] `Runner.IsOpen(key)` and a late-bound open-state adapter for the temperatures check in pkg/health/runner/runner.go, pkg/health/check/temps/temps.go, cmd/health_hardware.go and cmd/health.go (tests in pkg/health/runner/runner_test.go: `TestIsOpen`; pkg/health/check/temps/temps_test.go with a fake `OpenState`; cmd/health_hardware_test.go: `TestOpenStateLateBound`) (FR-019, R13)
    - `Runner.IsOpen(key) bool` answers from `Engine.Condition(key)` (`ok && Open`)
    - `temps.OpenState` interface `{ IsOpen(key string) bool }` (named apart from `Engine.OpenConditions` and `runner.OpenCondition`); `temps.New(hwmon, nvidia, cfg, open OpenState)`, with its call site in `buildTempsCheck` and the existing tests' calls changed in this task
    - fx builds the checks before the runner, so cmd passes an `openState` adapter that answers `false` until bound; `newHealth` binds it to the runner right after `runner.New`, next to `reports.bind(r)`
- [x] T044 [US4] Unreadable versus gone, "gone" only when open, GPUs that stop reporting in pkg/health/check/temps/temps.go (depends on T039) (tests in pkg/health/check/temps/temps_test.go: `TestUnreadableKeepsProblem` proves US4 acc. 4 and SC-002, `TestGPUNoDevicesKeepsState` US4 acc. 5 and SC-002, `TestGoneWithoutProblemSilent` US4 acc. 6, `TestSensorWeekQuiet` SC-009) (FR-018, FR-019, R13)
    - A sensor with `Err` ⇒ Unknown reading (checker error `<key>/unreadable`), state and any open problem kept
    - Gone only when the sensor key and its chip directory are both absent from the read; listed in `Forget`; the `<sensor> gone` Event only when `OpenState.IsOpen(key)`; otherwise dropped silently
    - A GPU in the seen set missing from an `ErrNoGPUs` answer or reported with a nil temperature ⇒ Unknown with checker error `temps/gpu/<n>/unreadable`, state kept
    - `TestSensorWeekQuiet`: seven days of normal readings on a fake clock (16 per-core inputs, an unlisted board sensor without crit, a sensor that disappears without an open problem) produce zero alerts
- [x] T045 [US4] `/temp` lines in pkg/health/report/readings.go and the per-serial limits in cmd/health_hardware.go (tests in pkg/health/report/readings_test.go and cmd/health_hardware_test.go) (FR-015, FR-016, FR-017, FR-018, contracts/telegram-commands.md `/temp`)
    - `<sensor>: <now>°C (7d min/avg/max a/b/c)[, limit <n>°C]` and `<sensor>: unreadable since <time>`; CPU one line per package (`coretemp package-id-0`, `k10temp tdie`); listed board sensors with their limit, unlisted ones with their crit when exposed
    - `limits_by_serial` reaches the disks check through the temperatures section it is built with

**Checkpoint**: US1–US4 work independently; quickstart §4 passes

---

## Phase 7: User Story 5 - Network and service checks that judge what matters (Priority: P5)

**Goal**: One network condition with no route as router down, endpoint redirects, missing
containers, auth refusals and placeholders, definitive DoH answers, command output patterns and
timeout mode, future-dated stamps and an HTTP freshness source

**Independent Test**: With fake network, endpoint, DNS-over-HTTPS, command and file sources,
simulate a missing default route, an outage whose cause moves from router to internet, a redirect,
a probe whose output contains a forbidden pattern, a stamp one day in the future, and a no-such-name
answer (quickstart §5)

- [x] T046 [P] [US5] One `network/connectivity` condition with no route as router down in pkg/health/check/network/network.go, `KeyNetwork` in pkg/health/runner/runner.go and a gateway adapter in cmd/health_adapters.go (tests in pkg/health/check/network/network_test.go: `TestNoRouteIsRouterDown` proves US5 acc. 1, `TestCauseShiftOneOutage` US5 acc. 2 and SC-008, `TestFormerKeysForgotten`; pkg/health/runner/runner_test.go: `TestOfflineFromConnectivity`; cmd/health_test.go: `TestGatewaySourceMapsNoRoute`) (FR-033, FR-034, R21)
    - Bad texts `No usable default route` (the pure sentinel `network.ErrNoRoute`), `Router <addr> is not answering`, `Internet is unreachable (router <addr> answers)`; Good `Network is reachable`; a changing cause is a Bad reading on the same streak
    - A failure to read `/proc/net/route` or to ping stays unreadable; `network/router` and `network/internet` are listed in `Forget` every run (cleared silently)
    - Runner: `KeyRouter` / `KeyInternet` become `KeyNetwork`; `offline` = the connectivity condition is bad
    - cmd: `buildNetworkCheck` gets a `gatewaySource` over `*sysfs.Reader` that maps `sysfs.ErrNoDefaultGateway` to `network.ErrNoRoute`
- [x] T047 [US5] Runner `networkUnknown` and the network threshold default in pkg/health/runner/runner.go and pkg/health/config/defaults.go (tests in pkg/health/runner/runner_test.go: `TestNetworkUnknownSkipsInternetChecks`, `TestNetworkThresholdDefault`; pkg/health/config/config_test.go) (FR-035, FR-036, R21)
    - `networkUnknown` = the last network run returned `Err` or no reading ⇒ `needs_internet` checks are not run (Telegram still attempted)
    - Changed default: `network.threshold` `0` → the network `interval` (`5m`) when absent; `0` stays valid; mode PROBLEM by SMS, RESOLVED `tlg-or-sms` (unchanged)
- [x] T048 [US5] Service config keys in pkg/health/config/types.go, pkg/health/config/checks.go, pkg/health/config/defaults.go and pkg/health/config/validate.go (tests in pkg/health/config/config_test.go) (FR-037, FR-039, FR-041, FR-042, FR-043)
    - `http[].follow_redirects` bool, default `false` (changed from followed, up to 5); `http[].auth_refusal` `down` | `checker_error`, default `down`, "one of"
    - `http[].header.placeholders` and `freshness[].header.placeholders` [string], default `[]`, "non-empty entries"
    - `freshness[].future_tolerance` duration, default `1h`, "≥ 0"; `freshness[].url` URL, http(s), "exactly one of `path`, `url`"; "`url` requires `content: epoch`"; `freshness[].timeout` default `15s`, "> 0, ≤ `20s`; only with `url`"; `freshness[].header` `{name, value_env, prefix, placeholders}`, "as `http[].header`; only with `url`"
    - `command[].expect_output` / `forbid_output` regexp, "compiles (Go RE2)"; `command[].timeout_verdict` `broken` | `unreadable`, default `broken`
- [x] T049 [P] [US5] `ErrNotFound` for a missing container in pkg/host/docker/docker.go (tests in pkg/host/docker/docker_test.go) (FR-038, R22)
    - `Inspect` wraps `docker.ErrNotFound` when the inspect fails and a re-list filtered by name finds no container; the containerd errdefs helper is not used
- [x] T050 [P] [US5] Definitive negative DoH answers in pkg/doh/doh.go, `httpcheck` sentinels in pkg/health/check/httpcheck/dial.go and the `dohResolver` adapter in cmd/health_services.go (tests in pkg/doh/doh_test.go: `TestNXDomain`, `TestNoData`, `TestServfail`; cmd/health_services_test.go: `TestDoHResolverMapsErrors`) (FR-040, R23)
    - `doh.ErrNXDomain` (RCode `NameError`) next to `doh.ErrNoAddresses`; definitive only when both A and AAAA were answered: NXDOMAIN from either ⇒ `ErrNXDomain`, both empty ⇒ `ErrNoAddresses`; transport error, non-200, SERVFAIL or REFUSED ⇒ an ordinary error
    - `httpcheck.ErrNameNotFound` and `httpcheck.ErrNoAddress` are declared (judged in T053)
    - cmd: `httpChecks` passes `dohResolver{doh.New(...)}`, which maps `doh.ErrNXDomain` ⇒ `httpcheck.ErrNameNotFound` and `doh.ErrNoAddresses` ⇒ `httpcheck.ErrNoAddress` and passes every other error through
- [x] T051 [P] [US5] HTTP stamp adapter in pkg/host/httpstamp/httpstamp.go (tests in pkg/host/httpstamp/httpstamp_test.go, new, with `httptest`: `TestFetch`, `TestNoRedirect`, `TestBodyCap`) (FR-043, R26)
    - `Fetch(ctx, url, headerName, headerValue string) (status int, body []byte, err error)`: `GET` with `net/http` and an injectable transport, never follows redirects, returns at most 4 KiB of body; no parsing or judgement; outside the coverage gate like every `pkg/host/*` adapter
- [x] T052 [US5] Redirects, missing containers, auth refusals and extra placeholders in pkg/health/check/httpcheck/httpcheck.go, pkg/health/check/httpcheck/secret.go and the `lazyDocker` adapter in cmd/health_services.go (depends on T048, T049, T050) (tests in pkg/health/check/httpcheck/httpcheck_test.go: `TestRedirectNotFollowed` proves US5 acc. 3, `TestMissingContainerDown`, `TestAuthRefusalCheckerError`, `TestExtraPlaceholder`; cmd/health_services_test.go) (FR-037, FR-038, FR-039, R22)
    - `follow_redirects: false` ⇒ `checkRedirect` returns `http.ErrUseLastResponse` and the first response is judged against `status` (default 200–399); `true` keeps 001 behaviour
    - `httpcheck.ErrContainerNotFound` ⇒ Bad `container <name> does not exist`; cmd `lazyDocker.ContainerHealthy` maps `docker.ErrNotFound` to it
    - `auth_refusal: checker_error` and 401/403 ⇒ no reading and `CheckError` `http/<name>/auth` (`endpoint refused the credentials (<status>)`)
    - `header.placeholders` are extra case-insensitive markers for `looksLikePlaceholder`, on top of the built-in words; the secret checks are exported as one helper (`httpcheck.HeaderSecret`) that the freshness check reuses
- [x] T053 [US5] NXDOMAIN and NODATA judged down in `failure` in pkg/health/check/httpcheck/httpcheck.go and pkg/health/check/httpcheck/dial.go (depends on T050) (tests in pkg/health/check/httpcheck/httpcheck_test.go: `TestDoHNXDomainDown`, `TestDoHNoDataDown`, `TestDoHUnreachableUnknown`, prove US5 acc. 6) (FR-040, R23)
    - `ErrNameNotFound` ⇒ Bad `name <host> does not exist`; `ErrNoAddress` ⇒ Bad `has no address`; every other resolver error stays unreadable
- [x] T054 [US5] Probe output: capture in pkg/host/execprobe/execprobe.go, judge in pkg/health/check/command/command.go, call site in cmd/health_services.go (depends on T048) (tests in pkg/host/execprobe/execprobe_test.go; pkg/health/check/command/command_test.go: `TestForbidOutput` proves US5 acc. 4, `TestExpectOutput`, `TestTimeoutUnreadable`; cmd/health_services_test.go) (FR-041, R24)
    - `command.Runner.Run` returns `(exitCode int, output []byte, err error)`; `execprobe.Runner` captures stdout and stderr together, capped at 64 KiB (excess discarded), never logged; cmd keeps passing `execprobe.Runner{}`
    - Verdict order: "start failure ⇒ checker error; timeout ⇒ `timeout_verdict`; exit ≠ 0 ⇒ Bad; `forbid_output` match ⇒ Bad; `expect_output` no match ⇒ Bad; else Good"; `unreadable` timeout ⇒ Unknown with the fix strike count unchanged; texts name the pattern key (`output matched forbid_output`), never the output
- [x] T055 [US5] Future-dated stamps in `judgeAge` in pkg/health/check/freshness/freshness.go (depends on T048) (tests in pkg/health/check/freshness/freshness_test.go: `TestFutureStamp` proves US5 acc. 5, `TestFutureWithinTolerance`) (FR-042, R25)
    - Signed age; a stamp more than `future_tolerance` ahead of now is Bad `is dated <duration> in the future`; within the tolerance it is fresh; applies to `epoch`, `mtime` and the HTTP source
- [x] T056 [US5] HTTP stamp source, pure side, in pkg/health/check/freshness/stamp.go and pkg/health/check/freshness/freshness.go (depends on T052) (tests in pkg/health/check/freshness/stamp_test.go, new: `TestHTTPStamp`, `TestHTTPNoAnswerUnknown` proves US5 acc. 7, `TestStampHeaderSecret`) (FR-043, R26)
    - New `freshness.StampSource` interface `Fetch(ctx, url, headerName, headerValue string) (status int, body []byte, err error)` (the existing `freshness.Source` stays the file interface) and a variadic `WithStampSource(StampSource)` option, so `freshness.New` keeps its parameters
    - The fetch is bounded by `health.Bound` at the entry's `timeout`; the header value comes from `httpcheck.HeaderSecret` (environment lookup, placeholders checked); the trimmed body parses as Unix seconds (integer, or decimal with the fraction ignored)
    - No answer, non-2xx or an unparsable body ⇒ Unknown with `CheckError` `freshness/<name>/unreadable` (`stamp endpoint did not answer` / `answered <status>` / `body is not a Unix time`), so no problem opens or resolves
- [x] T057 [US5] Wire endpoint options, the DoH adapter, the HTTP stamp source and the output-capturing runner end to end in cmd/health_services.go (depends on T051, T054, T056) (tests in cmd/health_services_test.go) (FR-037, FR-039, FR-040, FR-041, FR-043)
    - `freshnessChecks` passes `WithStampSource` over `httpstamp` for entries with `url`; the `dohResolver` of T050 is exercised through a configured DoH endpoint check
    - Header secrets named by `freshness[].header.value_env` are ordinary environment variables, as for `http`; no new environment variable

**Checkpoint**: US1–US5 work independently; quickstart §5 passes (and the optional smoke run of §7)

---

## Phase 8: User Story 6 - A weekly summary that shows the whole host (Priority: P6)

**Goal**: Drive lines with mounts, temperature, self-test, wear and asleep/unreadable marks; one
services line per check or group with freshness ages and the modem probe; a defined cut order

**Independent Test**: Seed state with drives (one asleep, one unreadable, one SATA SSD reporting
attribute 177), mounts, freshness stamps of various ages and a modem probe result; build the
summary with and without a length overflow; verify each line and the cut order (quickstart §6)

- [x] T058 [P] [US6] Raw mount and block topology reads in pkg/host/sysfs/sysfs.go (tests in pkg/host/sysfs/sysfs_test.go: `TestMountInfo`, `TestBlockTopology`) (FR-044, R27)
    - `MountInfo()` returns the `/proc/self/mountinfo` entries (device number, mount point) and `BlockTopology()` returns each block device's partitions and `holders` (md, dm) from `/sys/block`; raw data only, no aggregation; tested on literal fixtures and a temp-dir tree
- [x] T059 [P] [US6] `summary_group` common key in pkg/health/config/types.go, pkg/health/config/checks.go and pkg/health/config/validate.go (tests in pkg/health/config/config_test.go) (FR-046)
    - Every check: `summary_group` string, default none (one summary line per check), "`summary_group` matches `[a-z0-9-]+`"
- [x] T060 [P] [US6] Keep freshness stamps for the summary in pkg/health/check/freshness/freshness.go (tests in pkg/health/check/freshness/freshness_test.go) (FR-047, R27)
    - The existing `KVState` (the `State` passed to `freshness.New`) is extended to hold per entry `freshness.<name>.stamp` (Unix seconds) and `freshness.<name>.read_ok` (`0`/`1`), updated by every run
- [x] T061 [P] [US6] Pure mount aggregation per drive in pkg/health/report/mounts.go (tests in pkg/health/report/mounts_test.go, new: `TestDriveMountsPartitions`, `TestDriveMountsHolders`) (FR-044, R27)
    - `DriveMounts(node, entries, topology)` returns the mount points of the drive, its partitions and their holders, sorted; empty ⇒ `-`; a read error passed in ⇒ `?`
- [x] T062 [US6] Drive lines in pkg/health/report/summary.go (tests in pkg/health/report/summary_test.go: `TestSummaryDriveLine` proves US6 acc. 1, `TestSummaryAsleepSince` US6 acc. 2, `TestSummarySATAWear` US6 acc. 3) (FR-044, FR-045, FR-009)
    - `sda (/, /home) 41°C, self-test passed 12 d ago, wear 8%`: device node; mounts from `DriveMounts` ("`-` none, `?` unknown"); temperature; newest self-test result (`passed`, `failed`, `aborted`, `in progress`, `not readable`, `none`) and the calendar age of the last passed test; wear (NVMe or SATA SSD)
    - `asleep since Tue 14:00` / `unreadable since …` / `missing` when the last attempt did not read the drive
- [x] T063 [US6] Services lines with groups, freshness ages and the modem probe in pkg/health/report/summary.go (depends on T059, T060) (tests in pkg/health/report/summary_test.go: `TestSummaryFreshnessAge` proves US6 acc. 4, `TestSummaryModemProbe` US6 acc. 5) (FR-046, FR-047, FR-048, R27)
    - One line per configured check or per `summary_group`, including network, UPS and the modem probe: `ok` or the open problems' texts joined with `; `
    - Freshness ages: `<1 h`, `N h` below 48 h, `N d`, `none` when missing, `?` when unreadable; prefixed by the check name inside a group
    - Modem probe: `last round trip <age> ago` or `never`, or its failure text, and whether a probe is outstanding (`modem_probe: last round trip 3 d ago, no probe outstanding`)
- [x] T064 [US6] Cut order to one message in pkg/health/report/summary.go (tests in pkg/health/report/summary_test.go: `TestSummaryCutOrder`, `TestSummaryOpenProblemsKept`, prove US6 acc. 6 and SC-010) (FR-049, R27)
    - Services first (ok lines fold into `… and N more ok`, then lines from the end become `… N more`), then drives (`… N more drives`), then open problems with a trailing `…`; the open-problems section is never dropped and stays whole while it fits; the result fits 4 096 UTF-16 units with the host prefix
- [x] T065 [US6] Feed the summary from drive status, mounts, freshness kv, groups and the probe state in cmd/health_reports.go and cmd/health_services.go (depends on T058) (tests in cmd/health_reports_test.go and cmd/health_services_test.go) (FR-044, FR-045, FR-046, FR-047, FR-048)
    - `disks.Check.Status()` plus `report.DriveMounts` over `sysfs.MountInfo()` and `sysfs.BlockTopology()` at report time (read error ⇒ `?`); freshness state persisted through a `KVPersister`; `modemprobe.State` for the probe line

**Checkpoint**: All user stories functional; quickstart §6 passes

---

## Phase 9: Polish & Cross-Cutting Concerns

**Purpose**: Example configuration, documentation and the full gate

- [x] T066 [P] Document every new key and changed default in config/health.example.yaml (tests in pkg/health/config/config_test.go: the example still loads strictly) (contracts/config.md)
    - New keys: `pause_in_quiet_windows`, `summary_group`, `general.maintenance_max_hold`, `general.telegram_failing_after`, `disks.spare_floor_percent`, `disks.force_read_after`, `disks.include_serials`, `disks.extra_counters`, `disks.extra_counters_by_serial`, `temperatures.limits_by_serial`, `temperatures.board_sensors`, `http[].follow_redirects`, `http[].auth_refusal`, `header.placeholders`, `freshness[].future_tolerance`, `freshness[].url`, `freshness[].timeout`, `freshness[].header`, `command[].expect_output`, `command[].forbid_output`, `command[].timeout_verdict`
    - Changed defaults, each commented with the value that restores 001: `temperatures.threshold` 0, `limits.cpu` 90, `limits.drive_nvme` 60, `limits.drive_sata` 60, `network.threshold` = interval, `follow_redirects` false, hardware checks not set to pause in quiet windows (`pause_in_quiet_windows: true` restores 001); `disks.selftest_max_age` 40d is commented as calendar days, with `8d` the closest value to 001 but not an exact restore (001 counted power-on time)
    - Anonymised values only (`TEST000…` serials, `example.org`)
- [x] T067 [P] Update the health section of README.md and the package map in .claude/CLAUDE.md
    - README: new keys and changed defaults, the quiet-window change for hardware checks (`pause_in_quiet_windows`), the forced daily read of sleeping drives (at most one per run), the SMS fallback and Telegram-failing email, `journalctl` as an optional host tool (absent ⇒ the previous boot's tail is skipped with a log line), the one-shot CLI changes
    - CLAUDE.md: add `journal` and `httpstamp` to the `pkg/host/*` row; `COVERPKG` text unchanged (no new pure package outside `pkg/health/...`)
- [x] T068 Run the full gate (Taskfile.yml, .github/workflows/ci.yml), the success-criteria traceability over specs/002-deeper-health-checks/quickstart.md and the scans (depends on T066, T067)
    - `task lint` (0 issues); `task test:coverage` with the race detector, total over `COVERPKG` ≥ 80%; `CGO_ENABLED=0 task build`; `COVERPKG` in `Taskfile.yml` and `-coverpkg` in `.github/workflows/ci.yml` still identical
    - Run every `go test -run` line of specs/002-deeper-health-checks/quickstart.md §1–§6 (SC-011)
    - SC-012 traceability checklist: confirm each failure class has a passing test with recorded input that ends in an alert, and record the mapping in the commit message: each drive counter `TestBuiltinCounterRise`; spare `TestSpareFloor`, `TestSpareAtDriveThreshold`; wear `TestSATAWear`; self-test `TestSelfTestCalendarAge`, `TestFailureThenPass`; temperature classes `TestDriveTempAtLimit` (NVMe), `TestDriveTempBySerial` (SATA/SAS), `TestPackageOnly` (CPU), `TestGPUAtLimit` (GPU), `TestBoardCrit` (board); kernel classes `TestGHESRecord`, `TestIOErrorAnywhere`, `TestATALinkResetAnyLevel`, `TestPreviousBootTail`, `TestStillHappeningWithoutNewRecord`; memory `TestUncorrectableEveryRise`; network `TestNoRouteIsRouterDown`, `TestCauseShiftOneOutage`; service check types `TestRedirectNotFollowed`/`TestMissingContainerDown`/`TestDoHNXDomainDown` (http), `TestForbidOutput` (command), `TestFutureStamp`/`TestHTTPStamp` (freshness), the unchanged 001 tests of `containers`, `ups` and `modemprobe`; delivery failure `TestTelegramFailingEmailOnce`, `TestSMSFallbackProblem`, `TestDroppedProblemReopens`; a class without such a test is a blocker
    - SC-009 note: the simulated week is `TestSensorWeekQuiet` (T044), backed by `TestPackageOnly`, `TestUnlistedBoardNoCrit` and `TestGoneWithoutProblemSilent`; confirm it passes with zero alerts
    - Privacy scan of the unpushed diff and every `testdata/` fixture with the private-pattern list kept outside the repository; fix any hit before pushing (Principle VI)

---

## Dependencies & Execution Order

### Phase Dependencies

- **Phase 2 (Foundational)**: no prerequisite; blocks every user story. T001 → T002; T003 in
  parallel; T004 after T002 and T003. FR-028 sits here because the drive checker errors of US1
  need it (plan "Delivery phases"); migration 2 sits here because R28 serves every story and the
  one-shot CLI must handle a version-1 database from the first upgraded build.
- **Phases 3–8 (US1–US6)**: each depends only on Phase 2 and runs in priority order (one commit
  per phase).
- **Phase 9 (Polish)**: after every story phase.

### User Story Dependencies

- **US1 (P1)**: Phase 2 only.
- **US2 (P2)**: Phase 2 (T003 queue columns). The maintenance scenario uses a drive counter rise
  through a fake check, so it does not need US1.
- **US3 (P3)**: Phase 2 only.
- **US4 (P4)**: Phase 2 only; T040 edits `judgeTemp` in the disks check (after US1 when run in
  order, independent in content); T043 adds `Runner.IsOpen` to the runner.
- **US5 (P5)**: Phase 2 only.
- **US6 (P6)**: reads `disks.Check.Status()` (T014, US1) and modem probe state (001); without US1
  the drive section reads "unavailable" as in 001.

### Within Each Phase

- US1: T005 and T006 in parallel; T007 (also T005) → T008 → T009 → T010 → T011 → T012 → T013 →
  T014 → T015 → T016.
- US2: T017, T018, T019 in parallel; T020 → T021 (also T018) → T022 → T023 (also T017) → T024 →
  T025 → T026 (also T018) → T027 → T028.
- US3: T029 and T030 in parallel; T031 → T032 → T033 → T034 → T035 → T036 (also T029).
- US4: T037 and T038 in parallel; T039 → T040 (also T037) → T041 (also T038) → T042 → T043 →
  T044 (also T039) → T045.
- US5: T046, T049, T050, T051 in parallel; T047 → T048 (serialised: both edit
  pkg/health/config/defaults.go and config_test.go) → T052 (also T049, T050) → T053 (also T050) →
  T054 (also T048) → T055 (also T048) → T056 → T057 (also T051).
- US6: T058, T059, T060, T061 in parallel; T062 (also T061) → T063 (also T059, T060) → T064 →
  T065 (also T058).
- Polish: T066 and T067 in parallel → T068.

## Parallel Examples

```text
# Phase 2
T001 CheckError in pkg/health/check.go
T003 Migration 2 in pkg/health/store/

# Phase 3 (US1): config and drive source shape side by side
T005 Disks config keys in pkg/health/config/
T006 Raw path and wake flag through pkg/host/smartctl, disks.Source and cmd

# Phase 4 (US2)
T017 Engine.Reopen in pkg/health/engine.go
T018 Quiet and delivery config keys in pkg/health/config/
T019 Queue metadata in pkg/health/notify/notify.go and pkg/health/runner/persist.go

# Phase 5 (US3)
T029 Journal adapter in pkg/host/journal/journal.go
T030 Per-controller EDAC through pkg/host/edac, kernel.Counters and cmd

# Phase 6 (US4)
T037 Temperature config, T038 hwmon Device/Err through adapter, check type and cmd

# Phase 7 (US5)
T046 Network condition, T049 docker ErrNotFound, T050 DoH answers and httpcheck sentinels,
T051 httpstamp adapter

# Phase 8 (US6)
T058 sysfs raw reads, T059 summary_group key, T060 freshness stamp state, T061 report.DriveMounts

# Phase 9
T066 Example config, T067 README and CLAUDE.md
```

## Implementation Strategy

### MVP First (User Story 1 Only)

1. Phase 2: keyed checker errors and schema v2.
2. Phase 3 (US1): drives. **Stop and validate**: quickstart §1 green, `tgshd check --once` on a
   test host prints drive verdicts and `ASLEEP` lines without waking a drive.
3. Deploy: drive failure, the top concern, is fully covered.

### Incremental Delivery

1. US2 next: delivery and resolution protect every alert added before and after it.
2. US3 (kernel), US4 (temperatures), US5 (network and services), US6 (summary) in priority order;
   each phase is one commit that passes `task lint`, `task test:coverage` and the privacy scan on
   its own, and every task inside it leaves the build green.
3. Phase 9 closes with the example config, the docs, the success-criteria traceability and the
   full gate.

## Notes

- [P] tasks touch different files and need nothing unfinished in their phase.
- Each task cites its FRs; every FR-001…FR-049 is cited by at least one task.
- Commit subjects are plain imperative sentences, on `main`; nothing is pushed without the
  maintainer's request.
