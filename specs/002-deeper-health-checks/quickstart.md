# Quickstart: validating deeper health checks

Validation scenarios for 002. They use recorded, anonymised fixtures and fakes only: no real
drive, sensor, journal or service is needed, and nothing here contains real host data (serials
`TEST000…`, model `Example SSD`, hosts under `example.org`, addresses from RFC 5737, phone
`+10000000000`). Contracts: [config delta](contracts/config.md),
[Telegram delta](contracts/telegram-commands.md), [CLI delta](contracts/cli.md); state:
[data model](data-model.md); decisions: [research](research.md).

The test names below are the ones the tasks create (one per acceptance scenario, SC-011). Each
`go test -run` line is the scenario's runnable check.

## Prerequisites

- Go toolchain from `go.mod`, `task` (go-task).
- No root, no hardware: every source is a fake or a fixture under `testdata/`.

## 0. Gates

```bash
task lint            # 0 issues
task test:coverage   # race detector on; total coverage over COVERPKG ≥ 80 %
CGO_ENABLED=0 task build
```

Expected: all green; `COVERPKG` in `Taskfile.yml` and `-coverpkg` in `.github/workflows/ci.yml`
are still identical (no new package outside `pkg/health/...` holds decision logic).

## 1. Drives (US1)

```bash
go test ./pkg/health/check/disks/ -run 'TestBuiltinCounterRise|TestCounterNames|TestNVMeErrorLogSilent|TestHealthVerdict|TestSectionGaps|TestSelfTestClassification|TestSCSISelfTestUnsupported|TestNoVerdictKeepsProblem|TestSpareFloor|TestSpareAtDriveThreshold|TestMissingSection|TestSATAWear|TestNoTemperature|TestSelfTestCalendarAge|TestSelfTestGrace|TestFailureThenPass|TestUnsupportedSelfTestLog|TestSelfTestLogMissingKeepsState|TestForcedRead|TestForcedReadNotRetried|TestOneForcedReadPerRun|TestForcedReadFailureKeepsProblems|TestClassifyTransport|TestUSBSkipped|TestEmptyDiscovery|TestUpgradeUSBNotMissing' -v
go test ./pkg/host/smartctl/ -run 'TestDevicePath|TestWakeFlag' -v
go test ./cmd/ -run 'TestSmartctlSourceMapsNodes|TestOneShotAsleep' -v
```

| Scenario (fixture) | Expected |
|---|---|
| `sata_counters_183.json`: baseline 183 = 0, then 1, then 3 | one Event per rise (`runtime bad blocks (ATA 183) rose 0→1`, then `1→3`); same for each of 5, 179, 181, 182, 183, 187, 188, 195, 197, 198, 199 and NVMe media errors (SC-001) |
| operator counter 230 with no known name | `attribute 230 (ATA 230) rose …` |
| `nvme_spare_19.json`: threshold 10, spare 19 | `disks/TEST0000000001/spare` Bad, text names the 20 % floor |
| spare 9, threshold 10 | Bad, text names the drive threshold (`at or below its threshold 10%`) |
| `no_smart_status.json` with `disks/…/health` open | no reading for health, no RESOLVED, checker error `disks/<serial>/no-verdict` |
| `no_attribute_table.json` | counters keep their baseline; checker error `…/attributes` |
| fake source: node asleep at every run, clock +25 h | one `wake=true` read; status `last read` updates; a second forced read only after another 24 h (SC-003) |
| forced read fails, runs at +1 h and +23 h, then +24 h | no forced read at +1 h or +23 h; the next one at +24 h; checker error `disks/<serial>/forced-read`, problems kept (SC-002, SC-003) |
| three drives due at once | one forced read per run, oldest last read first; the others in the next runs |
| last pass newly observed 41 days ago (fake clock) | `disks/…/selftest` Bad, text `41 d` |
| drive first seen today, no pass, clock +40 d | no self-test PROBLEM |
| log gains a failure then a newer pass between runs | exactly one `selftest-failure` Event |
| `Drives()` lists a node whose path has a `usb` segment, `include_serials` empty | not read, not tracked, never missing |
| stored 001 baseline whose node now classifies as `usb`, serial not listed | retired silently with one log line; no `missing` PROBLEM |
| SAS drive with or without a self-test log | self-test `not readable`; no checker error |
| `Drives()` returns no node while 2 drives are known | checker error `disks/discovery/empty`; no `missing` PROBLEM |
| `nvme_errlog_rise.json` | no Event; sample `drive.<serial>.nvme_err_log` only |
| `sata_ssd_177.json`: normalised 92 | wear 8 %, judged against `wear_percent` |

## 2. Delivery and resolution (US2)

```bash
go test ./pkg/health/notify/ -run 'TestTelegramFailingEmailOnce|TestSMSFallbackProblem|TestResolvedBySMSWhenProblemWasSMS|TestChannelHealthTransitions|TestUndeliveredKeptUntilShown|TestUndeliveredLinesCarryOver|TestUndeliveredOverflowCounted|TestUndeliveredPrefix' -v
go test ./pkg/health/runner/ -run 'TestHardwareRunsInMaintenance|TestStuckMaintenanceLock|TestSnapshotPaused|TestDistinctCheckerErrors|TestStoreRecoveryNotice|TestBootGraceHardware|TestDroppedProblemReopens|TestDroppedProblemNewStreakListed|TestDroppedResolvedListed' -v
go test ./pkg/health/report/ -run 'TestQuietNoteListsPaused' -v
go test ./pkg/health/ -run 'TestReopen' -v
go test ./pkg/health/store/ -run 'TestMigrateV1ToCurrent|TestQueueKindRoundTrip' -v
```

| Scenario | Expected |
|---|---|
| fake Telegram fails for 20 min (fake clock), SMTP fake present | exactly one email `notify/telegram/failing` at 15 min + one tick; none after (SC-004) |
| PROBLEM queued while Telegram fails, then RESOLVED while still failing | both sent by the SMS fake; both still queued for Telegram |
| PROBLEM trimmed from a full queue while its condition stays open | next tick: condition not open; PROBLEM raised again (SC-005) |
| PROBLEM dropped after its condition resolved and a new streak started | listed; next delivered Telegram message starts with `Undelivered earlier:` |
| PROBLEM expired after its condition resolved (no longer tracked) | listed the same way (SC-005) |
| 60 PROBLEMs listed, a short message limit | 50 kept as items, `… and 10 more undelivered PROBLEMs` always shown; item lines that did not fit come with the next delivered message; nothing only logged |
| maintenance lock held (fake), drive counter rises, a container goes down | counter Event delivered; containers check not run (SC-006) |
| `/status` during that window | `Note: quiet window active (maintenance, held since …); paused: containers, …` |
| lock held for 6 h 1 min (fake clock) | checker error `health/maintenance/stuck` |
| one check returns errors A then B within 24 h | both delivered |
| checkpoint fails, then succeeds | one `CHECKER OK: health: state can be saved again` |
| database at `user_version = 1` with queued rows | migrates to 2; rows read back as `kind = event` |

## 3. Kernel and memory (US3)

```bash
go test ./pkg/health/check/kernel/ -run 'TestPreviousBootTail|TestNoPersistentJournal|TestBootTailIncomplete|TestGHESRecord|TestIOErrorAnywhere|TestATALinkResetAnyLevel|TestStillHappeningWithoutNewRecord|TestUncorrectableEveryRise|TestUnreadableController|TestLostRecords' -v
go test ./pkg/host/journal/ -run 'TestDecodeJSONLines|TestUnavailable|TestRecordCap' -v
go test ./pkg/host/edac/ -run 'TestPerControllerErr' -v
go test ./cmd/ -run 'TestJournalSourceMapsErrors' -v
```

| Scenario (fixture) | Expected |
|---|---|
| `journal_prev_boot.jsonl` (anonymised `journalctl -o json` lines), stored cursor `usec` mid-file | only the records after `usec` are classified; one `Kernel (previous boot): <class> found (<n> records). Last: <line>` Event for the machine-check class (SC-007) |
| fake journal returns `ErrNoJournal` | one log line, no checker error, cursor moves to the current boot |
| fake journal blocks past the 20 s bound, or stops at the record cap | records read are reported; checker error `kernel/boottail/incomplete`; cursor moves to the current boot; the next run does not read the tail again |
| records `{1}[Hardware Error]: …`, `EXT4-fs warning (device sda1): … I/O error …`, `ata3: SError: { … }` at level 6 | counted as machine-check, storage I/O, ATA |
| episode with pending counts, clock +6 h, no new record | one "still happening" Event |
| EDAC `ue_count` 0→1, then 1→2 within an hour | two Events |
| `mc1/ce_count` unreadable | `mc0` judged; checker error `kernel/mc1/edac-unreadable` |
| records with seq 10, 11, 15 | checker error `kernel/kmsg/lost`: 3 records |

## 4. Temperatures (US4)

```bash
go test ./pkg/health/check/temps/ -run 'TestPackageOnly|TestUnlistedBoardNoCrit|TestBoardCrit|TestImplausible|TestGPUAtLimit|TestUnreadableKeepsProblem|TestGPUNoDevicesKeepsState|TestGoneWithoutProblemSilent|TestSensorWeekQuiet|TestNoCPUSensor' -v
go test ./pkg/health/check/disks/ -run 'TestDriveTempAtLimit|TestDriveTempBySerial' -v
go test ./pkg/health/runner/ -run 'TestIsOpen' -v
go test ./pkg/host/hwmon/ ./pkg/host/nvidia/ -run 'TestUnreadableInput|TestSensorDevice|TestNoDevicesSentinel|TestNAKept' -v
go test ./cmd/ -run 'TestOpenStateLateBound|TestNvidiaSourceMapsNoDevices' -v
```

| Scenario | Expected |
|---|---|
| NVMe drive at 60 °C, defaults | PROBLEM on the first reading |
| coretemp `Package id 0` plus 16 `Core n` inputs, one core hot | one condition `temps/coretemp/package-id-0`, judged by the package value |
| unlisted `nct*` sensor at 110 °C, no `crit` | sample only, nothing sent (SC-009) |
| sensor reading −273 °C | Unknown, checker error, no PROBLEM |
| open problem, then the input returns `EIO` | checker error; problem stays open; no "gone" |
| GPU seen before, `temps.ErrNoGPUs` | checker error; GPU state kept |
| sensor without a problem whose chip vanished | dropped, no message |
| seven days of normal readings (fake clock): 16 core inputs, unlisted board sensor without crit, a sensor that leaves without a problem | zero alerts (SC-009) |
| `limits_by_serial: {TEST0000000002: 50}`, drive at 52 °C | PROBLEM for that drive only |

## 5. Network and services (US5)

```bash
go test ./pkg/health/check/network/ -run 'TestNoRouteIsRouterDown|TestCauseShiftOneOutage|TestFormerKeysForgotten' -v
go test ./pkg/health/runner/ -run 'TestOfflineFromConnectivity|TestNetworkUnknownSkipsInternetChecks|TestNetworkThresholdDefault' -v
go test ./pkg/health/check/httpcheck/ -run 'TestRedirectNotFollowed|TestMissingContainerDown|TestAuthRefusalCheckerError|TestExtraPlaceholder|TestDoHNXDomainDown|TestDoHNoDataDown|TestDoHUnreachableUnknown' -v
go test ./pkg/doh/ -run 'TestNXDomain|TestNoData|TestServfail' -v
go test ./pkg/health/check/command/ -run 'TestForbidOutput|TestExpectOutput|TestTimeoutUnreadable' -v
go test ./pkg/health/check/freshness/ -run 'TestFutureStamp|TestFutureWithinTolerance|TestHTTPStamp|TestHTTPNoAnswerUnknown|TestStampHeaderSecret' -v
go test ./pkg/host/httpstamp/ -run 'TestFetch|TestNoRedirect|TestBodyCap' -v
go test ./cmd/ -run 'TestGatewaySourceMapsNoRoute|TestDoHResolverMapsErrors' -v
```

| Scenario | Expected |
|---|---|
| gateway source returns `network.ErrNoRoute` | `network/connectivity` Bad, PROBLEM by SMS after one interval |
| router down, then router up and internet down, then all up | one PROBLEM, one RESOLVED (SC-008) |
| `httptest` server answering `302 Location: https://example.org/` | not followed; judged Good (status 302 in 200–399) |
| linked container fake returns `httpcheck.ErrContainerNotFound` | `http/<name>` Bad, "does not exist" |
| `auth_refusal: checker_error`, server answers 401 | no reading; checker error `http/<name>/auth` |
| fake resolver returns `httpcheck.ErrNameNotFound` / `httpcheck.ErrNoAddress` / connection refused | Bad / Bad / unreadable |
| probe exits 0 and prints `DEGRADED`, `forbid_output: "(?i)degraded"` | Bad |
| stamp = now + 1 day | Bad "in the future"; now + 5 min ⇒ fresh |
| fake stamp source: no answer | Unknown; no PROBLEM opens or resolves |

## 6. Weekly summary (US6)

```bash
go test ./pkg/health/report/ -run 'TestDriveMountsPartitions|TestDriveMountsHolders|TestSummaryDriveLine|TestSummaryAsleepSince|TestSummarySATAWear|TestSummaryFreshnessAge|TestSummaryModemProbe|TestSummaryCutOrder|TestSummaryOpenProblemsKept' -v
go test ./pkg/host/sysfs/ -run 'TestMountInfo|TestBlockTopology' -v
```

| Scenario (seeded state) | Expected line |
|---|---|
| drive with partitions mounted at `/` and `/home`, 41 °C, pass 12 days ago | `sda (/, /home) 41°C, self-test passed 12 d ago, …` |
| drive asleep since Tuesday | `… asleep since Tue …` |
| SATA SSD attribute 177 = 92 | `wear 8%` |
| freshness stamp 30 h old | `… 30 h` |
| modem probe last back 3 days ago | `modem_probe: last round trip 3 d ago, no probe outstanding` |
| 200 services and 5 open problems | services folded first; open problems complete; one message ≤ 4 096 units (SC-010) |

## 7. Optional smoke run without hardware

A scratch configuration exercises the freshness changes end to end with the real daemon loop and
no host data:

```bash
mkdir -p /tmp/tgshd-qs && date -d '+1 day' +%s > /tmp/tgshd-qs/stamp
cat > /tmp/tgshd-qs/health.yaml <<'EOF'
general: { state_dir: /tmp/tgshd-qs, heartbeat_file: /tmp/tgshd-qs/heartbeat }
freshness:
  - { name: future, path: /tmp/tgshd-qs/stamp, max_age: 2m, future_tolerance: 1h }
EOF
BOT_HEALTH_CONFIG=/tmp/tgshd-qs/health.yaml ./bin/tgshd check --once
```

Expected: `BAD freshness/future: … is dated 23h59m… in the future` (about one day), exit 0, nothing sent.
