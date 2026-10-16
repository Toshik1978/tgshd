# Research: Deeper Health Checks

Decisions taken while planning 002. Each entry: **Decision**, **Rationale**, **Alternatives
considered**. Numbering restarts at R1 for this feature; references to the 001 decisions are
written "001 R<n>" ([001 research](../001-host-health-monitoring/research.md)). Every decision is
grounded in the current code; file paths are given relative to the repository root. Nothing
host-specific is recorded here: fixtures use serials `TEST000…`, the model `Example SSD`, hosts
under `example.org` and addresses from RFC 5737.

No new third-party dependency is needed. Every decision below uses the standard library, the
dependencies already approved for 001, or an external tool that is optional at run time
(`journalctl`, like `smartctl` and `nvidia-smi` in 001).

---

## R1. Drive error counters and the NVMe spare floor (FR-006, FR-007, FR-008, FR-009)

- **Decision**:
  - Extend `smartJSON` in `pkg/health/check/disks/smart.go`: each `ata_smart_attributes.table[]`
    row also decodes `value` (the normalised value, `*int`) next to `raw.value`; the root decodes
    `rotation_rate` (`*int`, `0` = solid state).
  - The built-in ATA set (`trackedATA`) grows from {5, 187, 188, 197, 198, 199} to
    {5, 179, 181, 182, 183, 187, 188, 195, 197, 198, 199}. Each is compared by `raw.value`
    against the drive's own baseline, exactly as 001 does for the six it had (`judgeCounters`):
    one Event per rise, the baseline moves to the new value, a counter seen for the first time
    only sets the baseline. The Event text is `drive <serial> (<model>): <counter name> (ATA <id>)
    rose <a>→<b>`; `counterName` returns names that do not embed the id (the 001 names lose their
    `(ATA n)` suffix): 5 "reallocated sectors", 179 "used reserved blocks", 181 "program fail",
    182 "erase fail", 183 "runtime bad blocks", 187 "reported uncorrectable errors", 188 "command
    timeouts", 195 "hardware ECC", 197 "pending sectors", 198 "offline uncorrectable sectors",
    199 "UDMA CRC errors"; an operator-added id without a known name is `attribute <id>`. The NVMe
    counter reads `media errors (NVMe)`.
  - Operator counters (`disks.extra_counters`, `disks.extra_counters_by_serial`) are ATA ids
    merged into the tracked set per drive before decoding; they can only add. The set is computed
    in the check (`counterSet(serial)`), so `DecodeSMART` takes it as an argument.
  - NVMe: `media_errors` stays tracked (`CounterNVMeMediaErrors`). `num_err_log_entries`
    (`CounterNVMeErrorLog`) is removed from `Report.Counters` and becomes a sample
    `drive.<serial>.nvme_err_log` only; its stored baseline value is dropped on the next write so
    it can never alert again.
  - Spare: `aspectSpare` is Bad when `available_spare <= available_spare_threshold` **or**
    `available_spare < disks.spare_floor_percent` (default 20). Naming rule: when the spare is at
    or below the drive's own threshold the text names the drive threshold ("at or below its
    threshold <t>%"); otherwise it names the floor ("below the <f>% floor"), as in the acceptance
    case 19% < 20% with a 10% threshold. A drive that
    reports a spare but no threshold is judged against the floor alone (001 needed both).
  - SATA SSD wear: for `Bus == ATA`, `rotation_rate == 0` and attribute 177 present with a
    normalised `value` in 0–100, `Report.PercentageUsed = 100 - value` (clamped to 0–100). It
    then flows through the existing wear path (`aspectWear`, `drive.<serial>.wear` sample,
    `/disks`, summary).
- **Rationale**: `smartctl --json=c -a` already returns every field needed in the one exec per
  drive that 001 makes (001 R3). The rise-against-own-baseline rule from 001 means a counter
  that is already non-zero on a healthy drive never alerts until it grows, so a fixed built-in set
  is safe. Some drives pack several fields into the raw value of 188 or 195; as in 001 for 188 the
  raw value is used as is, and a drive whose counter climbs during normal operation is excluded by
  the operator (spec Assumptions). Attribute 177 is the wear-levelling count on SATA SSDs; gating
  on `rotation_rate == 0` keeps a rotating drive that reuses id 177 out of the wear judgement.
- **Alternatives considered**: judging normalised value against `thresh` (smartctl's own
  "prefail" view; it fires far too late for an early warning); masking 188/195 to their low
  16 bits (layouts differ per drive, a wrong mask hides real rises); keeping
  `num_err_log_entries` as an alert with a large step (still noise: the log records benign
  unsupported-command events).

## R2. Missing health verdict and missing sections (FR-004, FR-005, FR-012, FR-013)

- **Decision**: `DecodeSMART` returns, next to the `Report`, a `Gaps` value listing what the read
  lacked; the check judges only what is present and turns each gap into a keyed checker error
  (R6):
  - **Health verdict**: explicit when `smart_status.passed` is present, or when exit status bit 3
    (DISK FAILING) is set. Otherwise `Report.Health` is `unknown`: no reading for
    `disks/<serial>/health` (so an open problem stays open and nothing resolves) and checker error
    `disks/<serial>/no-verdict`. `Report.HealthFailed bool` becomes a tri-state.
  - **Sections** by protocol (`device.protocol`):
    - ATA needs `ata_smart_attributes.table`; absent ⇒ gap `attributes`: counters and wear are
      not judged and keep their baselines (`judgeCounters` is skipped, so no counter is seen as
      "gone").
    - NVMe needs `nvme_smart_health_information_log`; absent ⇒ gap `nvme-health-log`: critical
      warning, spare, wear and media errors are not judged.
    - Both need a self-test log (`ata_smart_self_test_log`, `nvme_self_test_log`); absent ⇒ gap
      `selftest-log`: the self-test condition gets no reading and the baseline's self-test fields
      stay.
    - SCSI/SAS needs nothing beyond the verdict in this feature; its self-test log
      (`scsi_self_test_*`) is not decoded, so a SCSI/SAS drive's self-test format is always
      `unsupported` (present or absent log alike) and never raises `selftest-log` or any other
      checker error (see below).
  - **Unrecognised self-test format** (FR-012): a log that is present but whose entries cannot be
    decoded (no `status.value` / `self_test_result.value`, an `extended`-only ATA log, or any SCSI
    log) sets `Report.SelfTestFormat = unsupported`. The drive is never judged for self-tests, no
    checker error is raised (it is a property of the drive, not a failed read), and `/disks` and
    the summary say `self-test: not readable`. The flag is kept in the baseline so status shows it
    while the drive sleeps.
  - **No temperature** (FR-013): neither `temperature.current` nor the NVMe log's `temperature`
    ⇒ checker error `disks/<serial>/no-temperature`, rate-limited per distinct key (R6).
- **Rationale**: In 001 `HealthFailed` defaults to false, so a partial read (for example a USB
  bridge that drops the SMART status) produces a Good health reading and resolves an open
  "health FAILED" problem — the exact hiding FR-004 forbids. Treating each section independently
  keeps the parts of the read that did arrive.
- **Alternatives considered**: failing the whole drive read on any gap (loses the counters and
  temperature that did arrive); a Unknown reading per aspect (the runner already turns a State
  Unknown into a checker error keyed `<key>/unreadable`, but with the verdict-less case named
  explicitly the message is clearer and the key is stable).

## R3. Self-test classification, failures reported once, calendar age (FR-010, FR-011)

- **Decision**:
  - **Classification** (`smart.go`): every log entry becomes `passed`, `aborted`, `in progress` or
    `failed`. ATA status nibble (`status.value >> 4`): 0 passed; 1, 2 aborted (host abort,
    reset); 15 in progress; every other code failed (3–8 are the documented failures; 9–14 are
    reserved and count as failed). NVMe `self_test_result.value`: 0 passed; 1, 2, 3 aborted
    (abort command, controller reset, namespace removed); 0xF unused entry (skipped); every other
    code failed. 001 skipped unknown codes, so a reserved code was silently ignored.
  - **Entry identity**: `(power-on hours, test type, result code)` per entry, newest first as the
    logs list them.
  - **New failures once**: the baseline keeps `SelfTestHead`, the identities of the newest up to
    5 entries seen last time. On a read, the entries above the first position where that head
    matches are new. Each new `failed` entry that is **not** the newest finished entry is one
    Event `disks/<serial>/selftest-failure` ("self-test failed (<description>) at <h> h; a newer
    test has passed since"). A new failed entry that **is** the newest finished one is reported
    by the existing State condition `disks/<serial>/selftest` (Bad while the newest finished test
    failed), so the same failure is never sent twice. When the head cannot be aligned (log
    cleared or wrapped past 5 entries) only entries newer than the stored newest power-on hours
    count as new; ATA log hours are 16 bits and wrap at 65536, so they are compared modulo
    65536 (as is the power-on age of the newest test shown in `/disks`). A drive seen for the first time takes its whole log as the head: history is not
    re-reported (001's inventory rule), but a failed newest test still opens the State condition.
    A log read empty is marked seen (`selftest_seen`), so every entry that appears in it later
    is new: a new drive's first test counts as observed.
  - **Calendar age**: the baseline field `selftest_ok_at` (already persisted by 001, Unix
    seconds) becomes authoritative: it is set to `now` whenever a `passed` entry is newly observed
    by the head rule above. The age is `now - max(selftest_ok_at, first_seen_at)`; with no passed
    test ever observed the drive's `first_seen_at` is the start (first-sighting grace). The
    condition `disks/<serial>/selftest` is Bad when the newest finished test failed or the age
    exceeds `disks.selftest_max_age` (default now 40 d). Its text gives the age in days. The
    sample `drive.<serial>.selftest_age_h` keeps its name and now carries calendar hours.
  - Stored 001 baselines are read as is: their `selftest_ok_at` (set when the newest passed
    test's hours changed) is a valid "last newly observed" time, and an empty `SelfTestHead`
    aligns on the next read like a first sighting of the log (no Event storm).
- **Rationale**: power-on hours advance slowly on a drive that sleeps most of the day, so 001's
  "8 days of power-on hours" could mean weeks of calendar time; operators schedule tests by the
  calendar. Persisting "newly observed" rather than reading a timestamp from the drive is
  necessary because the self-test logs carry only power-on hours.
- **Alternatives considered**: converting power-on hours to calendar time with the drive's current
  power-on rate (unstable for drives that sleep irregularly); alerting every failed entry in the
  log on every read (repeats); comparing only the newest entry (misses a failure followed by a
  pass before the next check, acceptance scenario 7).

## R4. Sleeping drives and the forced read (FR-003)

- **Decision**:
  - `disks.Source.Run` gains a `wake bool`. `pkg/host/smartctl.Runner.Run` passes
    `-n standby,3` when `wake` is false (001) and `-n never` when true, which reads the drive
    whatever its power state.
  - The baseline keeps `LastSeenAt` (already: set only by a successful read) as "last successful
    read" and gains `asleep_since` (first run that found the drive asleep since its last read,
    cleared by a read).
  - The baseline also gains `forced_read_at` (Unix seconds of the last forced read attempt,
    successful or not). A node found asleep (exit 3) and attributed to a drive (`attribute`, 001)
    is due for a forced read when `now - max(last_seen_at, forced_read_at) >= disks.force_read_after`
    (default 24 h), so a forced read, successful or failed, is never retried before that age has
    passed again.
  - At most one forced read per disks run, done after the routine reads, serialised (never in
    parallel with another forced read), on the due drive whose `max(last_seen_at, forced_read_at)`
    is oldest; the other due drives wait for later runs. The read uses `wake=true` and sets
    `forced_read_at = now` before its result is judged. A forced read has its own per-drive timeout of 20 s,
    applied by the smartctl adapter whenever `wake=true`
    because a spun-down disk needs several seconds to spin up; it is
    still bounded by the run deadline through `health.Bound`. A forced read that fails is a
    checker error `disks/<serial>/forced-read`, the drive is marked `unreadable_since` and keeps
    every open problem.
  - An orphan sleeping node (no drive attributable, 001 `asleepSince`) is due after the same age
    (its in-memory asleep-since and forced-read times stand in for the baseline fields) and takes
    the same single forced-read slot, which identifies it; this takes the place of the 24 h
    in-memory suspension limit (`asleepOrphanLimit`) as the way an unknown sleeping node stops
    suspending the missing judgement.
  - `check --once` never wakes a drive: it prints `ASLEEP disks/<serial>: last read <age> ago`
    (contracts/cli.md).
- **Rationale**: one wake per drive per day costs little power or wear and bounds how long a
  drive's health is unknown (SC-003); recording the attempt, not only the success, keeps a drive
  that fails when woken from being woken on every run, and one forced read per run keeps spin-ups
  (and their power draw) apart and inside the run deadline. Doing the forced read inside the same run as the standby
  probe avoids a second schedule. Drives that cannot report a power state (NVMe) are read normally
  by smartctl and never take this path.
- **Alternatives considered**: a separate daily "wake all" job (wakes drives that were read
  recently anyway); `hdparm -C` before reading (another tool, same information as `-n standby`);
  never waking (a drive asleep for months is never checked).

## R5. Drive discovery by transport (FR-001, FR-002)

- **Decision**: `smartctl.Runner.Drives` returns `[]Node{Name, Path}`, where `Path` is the raw
  resolved sysfs device path (`filepath.EvalSymlinks("/sys/block/<node>")`); the adapter does not
  judge it. The 001 filters (`loop`, `ram`, `zram`, `dm-`, `md`, `sr`, `removable=1`, `size=0`)
  stay in the adapter. A cmd adapter maps it onto `disks.Node{Name, Path}`. The pure
  `disks.ClassifyTransport(path)` (in `pkg/health/check/disks`) derives the transport: a path
  segment `usb` ⇒ `usb`; `nvme`, `nvme-subsystem` or `nvme-subsysN` (native multipath) ⇒ `nvme`; `ata` ⇒ `sata`; `end_device-`, `sas_` or `port-` under
  a SCSI host ⇒ `sas`; anything else (virtio, MMC, FireWire, Thunderbolt bridges) ⇒ `other`.
  - The check reads `nvme`, `sata` and `sas` nodes. `usb` and `other` nodes are read only when
    `disks.include_serials` is non-empty, and a drive read from them is tracked only when its
    serial is listed; otherwise the read is discarded (never tracked, never missing).
  - An empty discovery (no node at all) while at least one active, non-excluded drive is known
    returns `Err` `disks/discovery/empty` ("no drives found while <n> are known; missing-drive
    check suspended") and no readings, so no "drive missing" opens.
  - Upgrade: a stored baseline whose drive now classifies as `usb` or `other` (from its stored
    `transport`, or, for a 001 baseline without one, from the classification of its stored node
    when discovered) and whose serial is not in `include_serials` is retired silently with a log
    line (`drive <serial> on a <transport> transport is no longer tracked; list it in
    include_serials to keep watching it`). It never opens "drive missing".
- **Rationale**: the sysfs path is the only transport signal available before reading the drive,
  needs only the standard library, and is stable across kernels. An external drive that leaves
  after a backup is not news.
- **Alternatives considered**: smartctl `device.type` (needs a read of every drive, and a USB
  bridge can report `sat`); `lsblk -J -o TRAN` (an exec for what sysfs already says).

## R6. Per-distinct checker-error keys (FR-028)

- **Decision**: add `health.CheckError{Subject, Kind string; Err error}` (`pkg/health/check.go`)
  implementing `error` and `Unwrap`. Checks join them into `CheckResult.Err`. `runner.apply`
  walks the joined tree (`errors.As` and the `Unwrap() []error` of `errors.Join`) and raises each
  `CheckError` under `<check>/<subject>/<kind>` (`<check>/<kind>` when the subject is empty, as
  for per-entry checks such as `http/<name>` whose name already is the subject); anything else
  falls back to the 001 key
  `<check>/run/error`. `RateMarks` already key on the full key and channel, so a new key gets
  through while another key of the same check is inside its 24 h window. Subjects are names from
  config, drive serials, sensor keys, controller names or channel names, never message text.
- **Rationale**: in 001 every joined error of a run shares `<check>/run/error`, so a second
  distinct fault of the disks check is silenced for 24 h behind the first one.
- **Alternatives considered**: keying on a hash of the message text (texts carry changing numbers,
  so one fault would re-alert on every change); one checker error per run with all texts (still
  one key).

## R7. Previous boot's tail from the journal; cursor across boots (FR-020)

- **Decision**:
  - New thin adapter `pkg/host/journal`. It runs
    `journalctl --boot=<boot id> --dmesg --output=json --output-fields=MESSAGE,PRIORITY,SYSLOG_FACILITY,_SOURCE_MONOTONIC_TIMESTAMP --no-pager --quiet`
    and streams stdout through `json.Decoder`, one object per line. `MESSAGE` is decoded as a
    string or, when journald stored non-UTF-8 bytes, as an array of numbers. The adapter stops
    after at most 200 000 records (`journal.ErrCapped`, mapped by cmd to `kernel.ErrJournalCapped`)
    and honours its context; it has no timer of its own.
  - The check bounds the read with `health.Bound` at `health.MaxCheckTimeout` (20 s), inside the
    40 s run budget. A read cut off by that deadline or by the record cap is incomplete: the
    records read so far are classified and reported, a checker error
    `kernel/boottail/incomplete` (`previous boot's kernel log tail was not read completely: <time
    limit | record cap>`) is raised, and the cursor moves to the new boot after that one attempt,
    so the tail is never retried on every run.
  - The kernel cursor (`kernel.Cursor`) gains `Usec`, the `Time` since boot of the last processed
    kmsg record. journald stores that same kmsg time as `_SOURCE_MONOTONIC_TIMESTAMP`, so the tail
    is every record of the stored boot with a timestamp greater than `Usec` (records whose
    timestamp equals `Usec` are compared by message too, so a tie is neither lost nor repeated).
  - At the first `Run` after start, when the stored `Cursor.BootID` differs from the current boot
    id, the check reads that tail **before** resetting the cursor, classifies it with the same
    class table (R8) and reports each class found as one Event ("Kernel (previous boot): …").
    The cursor moves to the new boot only after the tail was reported (or found incomplete, see
    above), so a crash during start reads the tail again rather than losing it.
  - No persistent journal: `journalctl` exits non-zero with "No journal files were found" or
    "Data from the specified boot … is not available", or the binary is absent (the Docker
    image). The adapter returns `journal.ErrUnavailable`, which the cmd adapter maps to the pure
    sentinel `kernel.ErrNoJournal` (pure packages never import `pkg/host`); the check writes one
    log line at start
    ("previous boot's kernel log tail could not be checked: no persistent journal") and continues
    with the current boot. It is not a checker error (spec: "written to the log").
- **Rationale**: Go journal readers need cgo (`sdjournal`, rejected in 001 R6). `journalctl -o
  json` is stable, present wherever a persistent journal exists, and selecting by boot id plus the
  kmsg monotonic time maps directly onto the cursor 001 already keeps, without storing a journal
  cursor that the kmsg path never sees.
- **Alternatives considered**: storing a journal `__CURSOR` (the daemon reads `/dev/kmsg`, not the
  journal, so it never learns one); reading the journal files directly (undocumented binary
  format); `--since` with wall-clock time (clock steps at boot make it unreliable); pstore/ramoops
  (only present on some hosts, and only for panics).

## R8. Kernel error patterns (FR-021)

- **Decision**: `classTable()` in `pkg/health/check/kernel/kernel.go` becomes ordered from
  specific to general (first match wins) and every class accepts any level:
  | Class | Added patterns (Go `regexp`, unanchored unless shown) |
  |---|---|
  | `kernel/machine-check` | `\[Hardware Error\]` anywhere (APEI/GHES: `{1}[Hardware Error]: Hardware error from APEI Generic Hardware Error Source…`, `[Hardware Error]: event severity: …`); `Machine check events logged`; `^mce: ` lines with `Machine Check` |
  | `kernel/nvme` | as 001, any level |
  | `kernel/ata` | `^ata\d+(\.\d+)?: SError: `; `^ata\d+(\.\d+)?: (hard \|soft )?resetting link`; `^ata\d+: (COMRESET failed\|link is slow to respond\|limiting SATA link speed)`; 001's `exception\|failed command\|hard resetting` — at any level (001 required warning or worse) |
  | `kernel/thermal-throttle` | as 001 |
  | `kernel/storage-io` (last) | `I/O error` anywhere, excluding records about `loop`, `sr`, `fd` and `zram` devices as the block layer and filesystems name them (`dev loop0`, `(device loop0)`, `XFS (loop0)`: `(\bdev \|\bdevice \|\()(loop\|sr\|fd\|zram)\d`) |
  `SATA link down` and `SATA link up` are deliberately not matched (empty ports at boot).
- **Rationale**: GHES records carry `[Hardware Error]` after a `{n}` prefix, so the 001 anchor
  `^mce: \[Hardware Error\]` misses them; the filesystem layers (ext4, XFS, Btrfs) report "I/O
  error" mid-line; link resets and SError reports are logged at info level. Putting the catch-all
  storage class last keeps NVMe and ATA records in their own classes.
- **Alternatives considered**: matching on `_KERNEL_SUBSYSTEM` (available only in the journal,
  not in `/dev/kmsg`); a severity floor (the records that matter here are often info-level).

## R9. Lost kernel records (FR-023)

- **Decision**: detection is pure, in `kernel.Check.Consume`: kmsg sequence numbers are
  contiguous within a boot, so a record whose `Seq` is greater than `maxSeq + 1` means
  `Seq - maxSeq - 1` records were overwritten before they were read (the adapter's `EPIPE` skip in
  `pkg/host/kmsg`). At start the first record of the current boot is compared with the stored
  cursor the same way (records lost while the daemon was down). Matching records dropped because
  the inbox was full (`maxInbox`) are counted too. `Run` raises one checker error
  `kernel/kmsg/lost` stating the count since the last report.
- **Rationale**: the gap is exact and needs no extra syscall; the adapter stays a thin reader.
- **Alternatives considered**: reporting `EPIPE` from the adapter (gives no count); enlarging the
  kernel ring buffer (host configuration, outside tgshd).

## R10. "Still happening" without new records; every uncorrectable rise (FR-022, FR-024)

- **Decision**: `Episode` gains `Unreported int`, the records added since the last message.
  `Run` walks every episode, not only those with new records: when `Unreported > 0` and
  `now - LastStillAt >= 6 h` it sends "still happening" with the counts and clears `Unreported`.
  EDAC uncorrectable (`ue`) rises bypass `episode` entirely: each rise is one Event
  "uncorrectable memory errors on <controller> rose <a>→<b>". Correctable (`ce`) rises keep the
  episode throttling of 001.
- **Rationale**: in 001 the "still" message is only evaluated while handling a new record, so
  counts gathered in a window wait for an unrelated later record. Each uncorrectable error may
  have corrupted data and is rare by nature, so no throttling is needed.
- **Alternatives considered**: a timer goroutine for episodes (the 30 s tick already provides one).

## R11. EDAC per-controller reads (FR-025)

- **Decision**: `pkg/host/edac.Reader.Read` returns every controller, each with its own `Err`
  (counter file missing or unparsable) instead of failing the whole read; `kernel.Counters` gains
  `Err error`. The check judges the readable controllers and raises
  `kernel/<controller>/edac-unreadable` per unreadable one, keeping its stored counters.
- **Rationale**: one bad sysfs file today blinds the check to every controller.
- **Alternatives considered**: per-DIMM counters (`dimm*/dimm_ce_count`): more detail for the
  message, but the controller totals already say which board area is failing; can be added later
  without changing the contract.

## R12. CPU package sensor selection (FR-016)

- **Decision**: in `temps.hwmonObserved`:
  - `coretemp`: only labels `Package id <n>` are judged, one condition per package
    (`temps/coretemp/package-id-<n>`, the 001 key, so stored conditions carry over). `Core <n>`
    labels are neither judged nor sampled.
  - `k10temp` and `zenpower`: one condition per chip instance, from `Tdie` when present, else
    `Tctl` (some CPUs report `Tctl` with a fixed offset; `Tdie` is the real die temperature).
    `Tccd<n>` labels are ignored. Instances are ordered by their hwmon device path, which the
    hwmon adapter now returns (`Sensor.Device`), so the second package keeps the `-2` suffix that
    001 assigns.
  - When the temperatures check is configured and a complete hwmon read yields no CPU package
    sensor, checker error `temps/cpu/none`.
  - A sensor that was judged in 001 but no longer is (a core sensor) is listed in `Forget` once:
    silently when it had no open problem (FR-019); with an open problem the alert log records
    `cleared` and the operator is told once by an Event "<sensor> is no longer judged
    separately; the package sensor covers it".
- **Rationale**: per-core readings repeat one fact many times and each core crossing the limit was
  a separate PROBLEM; the package sensor is what the CPU itself throttles on.
- **Alternatives considered**: the maximum of all cores (equals the package value within a degree
  on current CPUs, more code); keying by socket id from `/sys/devices/system/cpu` (more sysfs
  for the same result).

## R13. Board sensors, plausible range, unreadable versus gone, GPUs (FR-017, FR-018, FR-019)

- **Decision**:
  - Board sensors (any chip other than CPU, `nvme`, `drivetemp`) are judged only when listed in
    `temperatures.board_sensors` (chip and label, `*` glob allowed; optional per-entry `limit`,
    default `limits.board`), or, when unlisted, against the `temp<n>_crit` the sensor exposes.
    Unlisted sensors without a critical limit are sampled only.
  - Plausible range −40 °C to 150 °C: a value outside it is an Unknown reading (a checker error
    `<key>/unreadable` through the runner), never Bad.
  - `pkg/host/hwmon` keeps a sensor whose `temp<n>_input` exists but cannot be read (`EIO`,
    `ENODATA`, garbage) as `Sensor{Err: …}` instead of skipping it (`Celsius` is then
    meaningless); `temps.Sensor` gains the same `Device` and `Err` fields, so the cmd struct
    conversion keeps compiling. The check turns it into an
    Unknown reading: the state, and any open problem, is kept.
  - Gone: only a sensor key absent from a read in which its chip directory is also absent. A
    gone sensor is listed in `Forget`; the "gone" Event is emitted only when its condition is open.
    The check asks through a small `temps.OpenState` interface (`IsOpen(key) bool`, named apart
    from `Engine.OpenConditions` and `runner.OpenCondition`) passed at construction; `temps.New` also takes the whole `config.Temperatures` so `board_sensors`
    reaches the check. fx builds the
    checks before the runner, so cmd passes a late-bound adapter that answers `false` until
    `newHealth` binds it to the new `Runner.IsOpen(key)` (backed by `Engine.Condition`), the same
    way `healthReports.bind` hands over the runner. Otherwise it is dropped silently.
  - GPUs: `pkg/host/nvidia` returns `nvidia.ErrNoDevices` for "No devices were found" (it returned
    no GPUs and no error), which the cmd adapter maps to `temps.ErrNoGPUs`, and `Parse` keeps a GPU
    whose temperature is `[N/A]` with `Celsius == nil` (it was skipped); `temps.GPU.Celsius`
    becomes `*float64` with it, so the cmd struct conversion keeps compiling. The check: a GPU in the seen set that is now missing from a
    no-devices answer, or reported without a temperature, is an Unknown reading with a checker
    error, and its state is kept. An absent `nvidia-smi` still means no GPUs.
- **Rationale**: unlabelled board sensors often read nonsense (a value of −128 or 127 is a common
  "no sensor" marker), and in 001 a sensor that stops reading looked "gone" and resolved its open
  overheating problem silently through `Forget`.
- **Alternatives considered**: a blacklist of noisy sensors (open-ended; an allow-list matches the
  spec); treating `temp<n>_max` like crit (max is often a fan-curve point, not a limit).

## R14. Temperature limits, threshold and per-serial overrides (FR-014, FR-015)

- **Decision**: defaults in `pkg/health/config/defaults.go`: `drive_nvme` 60, `drive_sata` 60
  (now also used for SAS), `cpu` 90, `gpu` 85, `board` 75 (listed board sensors only),
  temperatures `threshold` 0. Comparisons become "at or above" (`>=`) in both
  `temps.apply` and `disks.judgeTemp`. New `temperatures.limits_by_serial` (serial ⇒ °C) is
  consulted first by `disks.judgeTemp`. With threshold 0 the disks check's own streak
  (`overFor`) returns 0 ⇒ the first hot reading is Bad, so its code path stays and keeps working
  for a non-zero threshold. A drive found asleep loses that streak: it wakes into a new one.
- **Rationale**: spec FR-014; 60 °C is the top of the operating range most drive datasheets give.
- **Alternatives considered**: per-model limits (model strings are real host data in fixtures and
  config; the serial is already the drive's identity everywhere).

## R15. Quiet windows do not pause hardware checks (FR-026)

- **Decision**: new common key `pause_in_quiet_windows` (bool). Its default is `false` for
  `disks`, `temperatures` and `kernel` and `true` for every other check (001 behaviour). The
  config layer resolves the default per section; `health.Check` gains `PauseInQuiet bool`. In
  `runner.Do` a quiet tick now runs the due checks with `PauseInQuiet == false` through the same
  `runChecks` path (network first is skipped because network pauses by default), then raises,
  flushes, dispatches and checkpoints as a normal tick. The kmsg reader always runs; only the
  kernel check's `Run` was gated. `/status` shows which checks the window pauses: the runner's
  `Snapshot` gains `Paused []string` (the configured checks with `PauseInQuiet`), and the quiet
  note lists them (contracts/telegram-commands.md `/status`).
- **Rationale**: a reboot caused by failing hardware starts a boot-grace window exactly when the
  hardware checks matter (SC-006).
- **Alternatives considered**: a fixed list of hardware checks with no per-check setting (the
  spec makes pausing in quiet windows a per-check setting, `pause_in_quiet_windows`); exempting
  only the kernel check (drives and temperatures fail during
  maintenance too).

## R16. Maintenance lock held too long (FR-027)

- **Decision**: the runner keeps `MaintenanceLock{HeldSince, LastSeenHeld}` in kv
  (`health.maintenance`). A tick that finds the lock held sets `HeldSince` if empty and updates
  `LastSeenHeld`; a tick that finds it free clears both. Continuity across a daemon restart is
  assumed only when `now - LastSeenHeld <= 5 min`; otherwise `HeldSince` restarts. When
  `now - HeldSince > general.maintenance_max_hold` (default 6 h), checker error
  `health/maintenance/stuck` ("maintenance lock held since <time> (more than <max>)"). The window itself stays: the
  lock may be a long but legitimate job, and hardware checks run anyway (R15).
- **Rationale**: a lock left by a crashed job silences the non-hardware checks forever in 001.
- **Alternatives considered**: ignoring the lock after the limit (would page for a legitimate long
  upgrade); reading the lock holder's PID (flock does not expose it portably).

## R17. Telegram delivery-failure tracking and the email checker error (FR-029)

- **Decision**: `notify.Notifier` keeps `ChannelHealth{FailingSince time; Reported bool}` per
  channel. A Telegram send that fails (any error other than `ErrPastDeadline`), or a Telegram
  message queued while the notifier is offline, sets `FailingSince` if empty; any successful
  Telegram send clears it and `Reported`. After each tick's deliveries the runner checks
  `now - FailingSince >= general.telegram_failing_after` (default 15 m) and, when not yet
  `Reported`, sends one email through the existing `Mailer` only (never Telegram), key
  `notify/telegram/failing`, text "Telegram delivery failing since <time>", and sets `Reported`.
  Without a mailer the same text is logged at error level. The state is persisted in kv
  (`notify.channels`) so a restart does not send it twice.
- **Rationale**: in 001 a Telegram outage is only visible through queued messages; email is the
  only channel that does not depend on Telegram or on the SMS modem being fed by Telegram.
- **Alternatives considered**: the checker-error path with its 24 h rate mark (it also posts to
  Telegram, which is the failing channel, and re-sends daily during a long outage, while the spec
  wants one per failure).

## R18. SMS fallback for PROBLEMs and their RESOLVED (FR-030)

- **Decision**:
  - `QueuedMessage` gains `Kind`, `Check` and `Key` (persisted, R28) and the notifier keeps a
    set `smsProblems` (condition keys whose PROBLEM went by SMS through the fallback), persisted
    in kv (`notify.sms_problems`).
  - When a PROBLEM is queued for Telegram while `FailingSince` is set (R17), or when Telegram
    starts failing with PROBLEMs already queued, each such PROBLEM is also sent by SMS once (plain
    form) and its key is added to `smsProblems`. The Telegram copy stays queued, so the chat
    history is complete once Telegram recovers (spec Assumptions).
  - A RESOLVED whose key is in `smsProblems` is sent by SMS as well as by its normal route, and
    the key leaves the set. Events and other RESOLVED messages wait for Telegram.
  - Checks whose own mode is `sms` or `tlg-or-sms` (network) keep their 001 routing; the fallback
    never sends a second SMS for a message that already went by SMS.
  - Without an SMS sender the PROBLEMs stay queued; the email of R17 is the only notice.
- **Rationale**: the clarification of 2026-10-16: RESOLVED goes by SMS only for PROBLEMs that were
  themselves sent by SMS. Keeping the set in the notifier (not in `health.Condition`) keeps the
  engine pure and channel-agnostic.
- **Alternatives considered**: a `NotifiedBySMS` column on `conditions` (couples the engine to a
  delivery channel); SMS for every message while Telegram fails (costly, noisy).

## R19. Undelivered PROBLEMs are never dropped silently (FR-031)

- **Decision**: the notifier's `OnDrop` (queue trimmed at `MaxQueue` or expired after 7 days)
  hands the runner the dropped item with its `Kind` and `Key`. For a dropped PROBLEM, three
  branches:
  - its condition still open ⇒ `Engine.Reopen(key)`: `Open = false`, `BadFor` kept, so the next
    Bad reading reaches the threshold at once and the PROBLEM is raised again;
  - its condition tracked but not open (it resolved and a new streak has started) ⇒ listed;
  - its condition no longer tracked (resolved meanwhile) ⇒ listed.
  Listing adds an `UndeliveredProblem{Key, Text, Since, DroppedAt}` to a kv list
  (`notify.undelivered`, `{Items, Folded}`). The next message delivered to Telegram is prefixed
  with "Undelivered earlier:" and one `PROBLEM: <text> (since …), no longer open` line per item,
  oldest first:
  - an item stays in the list until a delivered message has actually shown it; lines that do not
    fit the message limit stay queued for the next delivered message;
  - the list holds at most 50 items; a PROBLEM listed while it is full is not stored but
    increments `Folded`, shown as the counted line `… and N more undelivered PROBLEMs`, which is
    always part of the prefix (space for it is reserved before the item lines) and resets only
    after a delivery that showed it succeeds;
  - nothing is ever dropped to a log line only.
- **Rationale**: in 001 a dropped PROBLEM leaves its condition `Open = true`, so it never alerts
  again while the condition stays broken (spec Assumptions prefer re-raising).
- **Alternatives considered**: never dropping PROBLEMs from the queue (unbounded growth during a
  long outage); a separate "you missed" message (one more message to deliver while delivery is
  the problem); dropping the oldest listed item with a log line when the list is full (a log line
  never reaches the operator, FR-031).

## R20. State-write recovery notice (FR-032)

- **Decision**: the runner remembers that `health/store/write` was reported (`saveReported`).
  The first `Checkpoint` that succeeds afterwards pushes one Event "<host>: CHECKER OK: health:
  state can be saved again" (Telegram, and email when the failure went by email), clears
  `saveReported` and reverts the rate marks of `health/store/write` so a later failure is reported
  at once.
- **Rationale**: the operator was told persistence was broken; without the notice they cannot tell
  whether it still is.
- **Alternatives considered**: a RESOLVED through the engine (checker errors are not conditions;
  making them conditions would change every checker error's lifecycle).

## R21. Network: one condition; no route is the router down (FR-033 – FR-036)

- **Decision**:
  - `pkg/health/check/network` emits one State condition `network/connectivity`. Bad texts:
    "No usable default route" (the pure sentinel `network.ErrNoRoute`, which a cmd adapter maps
    from `sysfs.ErrNoDefaultGateway`), "Router <addr> is not answering",
    "Internet is unreachable (router <addr> answers)". Good text: "Network is reachable". The
    engine keeps one streak while the text changes, so a cause that moves from router to internet
    yields one PROBLEM and one RESOLVED.
  - A failure to read `/proc/net/route` or to ping stays `Err` (unreadable).
  - The 001 keys `network/router` and `network/internet` are listed in `Forget` by every run, so
    a condition stored by 001 is cleared silently after the upgrade.
  - Default threshold: when `network.threshold` is absent it equals `network.interval` (5 m);
    `0` stays valid. Mode: PROBLEM by SMS, RESOLVED `tlg-or-sms` (fixed, unchanged).
  - Runner: `offline` (silent Telegram queueing, skipping `needs_internet`) is "the connectivity
    condition is bad". New `networkUnknown`: the last network run returned `Err` or no reading.
    While it is set, `needs_internet` checks are not run (FR-036); Telegram is still attempted.
    `runner.KeyRouter`/`KeyInternet` become `runner.KeyNetwork`.
- **Rationale**: without a default route the host is offline; 001 returned `Err`, which never
  alerts. Two keys gave two PROBLEMs for one outage.
- **Alternatives considered**: keeping two keys and suppressing the second (needs cross-condition
  rules in the engine).

## R22. Endpoint checks: redirects, missing container, auth refusals, placeholders (FR-037 – FR-039)

- **Decision**:
  - New `follow_redirects` (default `false`). When false, `checkRedirect` returns
    `http.ErrUseLastResponse`, so the first response is judged against `status` (default
    200–399: a redirect is up). When true, 001 behaviour (at most 5, header stripped across
    hosts).
  - Missing container: `pkg/host/docker.Client.Inspect` wraps `docker.ErrNotFound` when the
    inspect fails and a re-list filtered by name finds no container (the re-list pattern already
    used by `inspect`); the cmd `lazyDocker` adapter maps it to the pure sentinel
    `httpcheck.ErrContainerNotFound`, which the check judges Bad "container X does not exist". No new dependency (the not-found helper of the containerd
    errdefs module is not promoted).
  - New `auth_refusal: down | checker_error` (default `down`, 001 behaviour). With
    `checker_error`, a 401 or 403 omits the condition and raises
    `http/<name>/auth` ("endpoint refused the credentials (401)").
  - New `header.placeholders` (list of strings): extra case-insensitive markers that make
    `looksLikePlaceholder` reject the secret, on top of the built-in words.
- **Rationale**: a redirect to a login page made broken services look up; a container that no
  longer exists was a checker error that never paged.
- **Alternatives considered**: judging the final response after redirects (the spec wants the
  first); a global placeholder list (markers are specific to how each secret is deployed).

## R23. DNS-over-HTTPS: NXDOMAIN and NODATA (FR-040)

- **Decision**: `pkg/doh` exports `ErrNXDomain` (RCode `NameError`) next to `ErrNoAddresses`
  (success with no A or AAAA). `Lookup` returns a definitive answer only when both queries were
  answered: NXDOMAIN from either ⇒ `ErrNXDomain`, except that addresses from either record type win over an
  NXDOMAIN from the other; both empty ⇒ `ErrNoAddresses`. Any transport
  error, non-200 status, SERVFAIL or REFUSED ⇒ an ordinary error. `httpcheck` defines its own
  sentinels `ErrNameNotFound` and `ErrNoAddress`; a cmd `dohResolver` adapter wraps `*doh.Resolver`
  and maps `doh.ErrNXDomain` / `doh.ErrNoAddresses` onto them (pure packages never import
  `pkg/doh`). `httpcheck.failure` treats those two as Bad ("name <host> does not exist" / "has
  no address") and everything else from the resolver as unreadable, as in 001.
- **Rationale**: NXDOMAIN and NODATA are answers: every user of the name fails. SERVFAIL means the
  resolver could not get an answer, which says nothing about the service.
- **Alternatives considered**: treating SERVFAIL as down (an upstream DNS hiccup would page as a
  service outage).

## R24. Command output patterns and timeout mode (FR-041)

- **Decision**: `command.Runner.Run` returns `(exitCode int, output []byte, err error)`;
  `pkg/host/execprobe` captures stdout and stderr together, capped at 64 KiB (excess discarded).
  The interface, the adapter and the cmd call site change together (cmd passes
  `execprobe.Runner{}` as `command.Runner` directly).
  New keys `expect_output` and `forbid_output` (Go RE2 regular expressions, compiled and validated
  at load). Verdict order: start failure ⇒ checker error; timeout ⇒ per `timeout_verdict`
  (`broken`, default, 001 behaviour; `unreadable` ⇒ Unknown reading, a checker error, the fix
  strike count unchanged); non-zero exit ⇒ Bad; `forbid_output` matches ⇒ Bad; `expect_output`
  does not match ⇒ Bad; else Good. Messages name the pattern key ("output matched forbid_output"),
  never the output.
- **Rationale**: probes that print trouble and exit 0 are common; output may contain secrets, so
  it is never put in messages or logs.
- **Alternatives considered**: shell-style globs (too weak for "contains one of"); separate
  stdout/stderr patterns (no use case in the spec).

## R25. Future-dated freshness stamps (FR-042)

- **Decision**: `freshness.judgeAge` gets the signed age; a stamp more than `future_tolerance`
  (default 1 h, per entry) ahead of now is Bad "is dated <duration> in the future". Within the
  tolerance it is fresh. Applies to `epoch` content, `mtime` and the HTTP source (R26).
- **Rationale**: a future stamp means a broken clock or writer and would hide a failing backup
  until that date.
- **Alternatives considered**: clamping future stamps to now (hides the broken writer).

## R26. Freshness from an HTTP endpoint (FR-043)

- **Decision**: a freshness entry takes either `path` (001) or `url` (new, mutually exclusive).
  With `url`: `timeout` (default 15 s, at most 20 s, bounded by `health.Bound`), optional
  `header` (same `Header` type as `http`, value from the environment, placeholders checked),
  `content: epoch` only. The check does `GET`, never follows redirects, reads at most 4 KiB and
  parses the trimmed body as Unix seconds (integer, or decimal with a fraction that is ignored).
  No answer, a non-2xx status or an unparsable body ⇒ Unknown (checker error
  `freshness/<name>/unreadable`): no problem opens or resolves. The fetch is I/O and lives in a
  new thin adapter `pkg/host/httpstamp` (outside the coverage gate, like the other host
  adapters): `Fetch(ctx, url, header name, header value) (status int, body []byte, err error)`
  with `net/http`, an injectable transport, no redirects followed and the 4 KiB cap, tested with
  `httptest`. The check sees it through the new `freshness.StampSource` interface (the existing
  `freshness.Source` stays the file interface). Resolving the header secret (environment lookup,
  placeholder markers), the status rule and parsing the body as a Unix time stay pure in
  `pkg/health/check/freshness`.
- **Rationale**: some jobs report their last success through a service. Judging only the stamp
  (not the service status) keeps the check about freshness; `http` entries judge availability.
- **Alternatives considered**: a JSON field path in the response (no need in the spec; can be added
  later as `json_field`); treating non-2xx as stale (would page for a monitoring endpoint outage
  as if the backup had failed).

## R27. Weekly summary layout and cut order (FR-044 – FR-049)

- **Decision**: `pkg/health/report/summary.go` builds four sections, each a list of lines:
  1. title;
  2. **Drives**: one line per active drive:
     `sda (/, /home) 41°C, self-test passed 12 d ago, wear 8%` — device node; mount points of the
     drive, its partitions and their holders (md, dm), `-` when none, `?` when unknown; current
     temperature; newest self-test result (`passed`, `failed`, `aborted`, `in progress`,
     `not readable`, `none`) and the calendar age of the last passed test; wear (NVMe percentage
     used or SATA SSD wear). A drive asleep or unreadable at the last attempt is marked
     `asleep since Tue 14:00` / `unreadable since …` (FR-045). Missing drives are marked
     `missing`.
  3. **Services**: one line per configured check, or per `summary_group` when the operator groups
     checks: `ok`, or the open problems' texts joined with `; `. Includes network, UPS and the
     modem probe. Freshness lines append the stamp age (`<1 h`, `N h` below 48 h, `N d`; `none`
     when missing; `?` when unreadable). The modem probe line shows `last round trip N d ago` or
     `never`, or its failure text, and whether a probe is outstanding (`no probe outstanding` /
     `probe outstanding`).
  4. **Open problems**: as `/status`.
  Cutting to the limit: first the services section (ok lines are folded into
  `… and N more ok`, then lines from the end become `… N more`), then the drives section the same
  way, then the open-problems section with a trailing ellipsis, which is never dropped.
  Inputs: drive status from the disks check (`disks.Check.Status()`, from the baseline), mount
  points from a pure helper `report.DriveMounts` (`pkg/health/report/mounts.go`) that aggregates
  raw data read by `pkg/host/sysfs` (`MountInfo()` returns the `/proc/self/mountinfo` entries,
  device number and mount point; `BlockTopology()` returns each block device's partitions and
  `holders` (md, dm) from `/sys/block`), freshness stamp ages from kv (`freshness.<name>.stamp`,
  `freshness.<name>.read_ok`) written by the check, modem probe state from `modemprobe.State`.
- **Rationale**: the summary must let the operator match each line to a physical drive and see
  every service, while staying one Telegram message (SC-010).
- **Alternatives considered**: two messages when long (the spec requires one); dropping drive
  lines before services (drives are the top concern).

## R28. Store schema migration (all stories)

- **Decision**: `migrations()` in `pkg/health/store/store.go` gains step 2 (`user_version = 2`):
  ```sql
  ALTER TABLE queue ADD COLUMN kind TEXT NOT NULL DEFAULT 'event';
  ALTER TABLE queue ADD COLUMN check_name TEXT NOT NULL DEFAULT '';
  ALTER TABLE queue ADD COLUMN key TEXT NOT NULL DEFAULT '';
  ```
  Everything else new is JSON inside existing columns: the drive baseline's `data` (R1–R4), kv
  values (kernel cursor with `usec`, `notify.channels`, `notify.sms_problems`,
  `notify.undelivered`, `health.maintenance`, `freshness.<name>.stamp`, `.read_ok`). Alert kinds
  stay within the 001 `CHECK` list (`event` for notices), so the `alerts` table is not rebuilt.
  Rows queued by 001 read as `kind = event`, which keeps them out of the SMS fallback and the
  re-raise rule; they are delivered as before. `OpenReadOnly` keeps requiring the current
  version; `check --once` against a database still at version 1 (daemon not yet restarted after
  the upgrade) runs without stored state and prints why (contract delta, `contracts/cli.md`).
  Downgrade is refused by the existing `ErrSchemaTooNew`.
- **Rationale**: `ALTER TABLE ADD COLUMN` with a constant default is a metadata-only change in
  SQLite, safe inside the migration transaction 001 already uses.
- **Alternatives considered**: new tables for channel health and undelivered problems (each is one
  small JSON value; kv already holds such state); rebuilding `alerts` to add kinds (a table copy
  for no user-visible gain).
