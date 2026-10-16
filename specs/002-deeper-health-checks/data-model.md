# Data Model: Deeper Health Checks

Amends the [001 data model](../001-host-health-monitoring/data-model.md). Everything not named
here is unchanged. In-memory structures stay authoritative; SQLite is their checkpoint. Times are
Unix seconds (UTC) in storage. Decisions are referenced as R<n> ([research](research.md)).

## Domain types (`pkg/health`)

### CheckError (new, R6)

| Field | Type | Notes |
|---|---|---|
| Subject | string | Drive serial, sensor key, controller, channel or a fixed component name (`discovery`, `kmsg`, `boottail`, `maintenance`, `cpu`); empty for per-entry checks (`http/<name>`, `freshness/<name>`, `command/<name>`) whose check name already names the subject; never message text |
| Kind | string | `[a-z0-9-]+`, e.g. `no-verdict`, `selftest-log`, `no-temperature`, `forced-read`, `edac-unreadable`, `lost`, `incomplete`, `auth`, `unreadable` |
| Err | error | Human text source; wrapped (`Unwrap`) |

Checker-error key: `<check>/<Subject>/<Kind>`, or `<check>/<Kind>` with no subject. Errors that are not a `CheckError` keep the 001
key `<check>/run/error`. Rate limiting (24 h per key and channel) is unchanged; it now applies per
distinct key (FR-028).

### Check (amended)

`health.Check` gains `PauseInQuiet bool` (from the common key `pause_in_quiet_windows`, R15).

### Condition (amended)

No new fields. New engine operation `Reopen(key)`: `Open = false`, `OpenedAt` cleared,
`StreakStart` and `BadFor` kept (R19).

```text
 Open ── PROBLEM dropped from the queue undelivered (R19) ──► Streak (BadFor ≥ threshold)
          the next Bad reading ⇒ PROBLEM again; a Good reading ⇒ Healthy, no RESOLVED
```

A dropped PROBLEM whose condition is tracked but not open (a new streak) or no longer tracked is
not reopened; it goes to the undelivered list (Delivery, below).

Network (R21): one condition `network/connectivity` whose text follows the cause; a change of
cause is a Bad reading on the same streak, never a new PROBLEM.

## Drive status and baseline (`pkg/health/check/disks`)

The stored `baselines` row is unchanged; its `data` JSON (`baselineData`) gains fields. Unknown
fields from a newer build are ignored; missing fields read as zero values.

### Drive baseline (extends 001 Baseline)

| Field (`data` JSON) | Type | Notes |
|---|---|---|
| `counters` | map[string]int64 | Keys `ata.<id>` for the built-in set {5, 179, 181, 182, 183, 187, 188, 195, 197, 198, 199} plus operator additions the drive reports, and `nvme.media_errors`. `nvme.num_err_log_entries` is dropped on the next write (R1) |
| `selftest_ok_at` | int64 | When a passed self-test was last **newly observed** (001 field, now authoritative for the age, R3) |
| `selftest_head` | []SelfTestID | Identities of the newest ≤ 5 log entries seen last time (R3) |
| `selftest_newest` | {result, description} | Newest finished entry, for status when the drive is not read |
| `selftest_format` | string | `ok` or `unsupported` (FR-012) |
| `node` | string | Device node last read from (001) |
| `asleep_since` | int64 | First run that found the drive asleep since its last read; 0 when awake |
| `forced_read_at` | int64 | Last forced read attempt, successful or not; 0 when never (R4) |
| `health` | string | `ok`, `failing` or `unknown`: the last read's verdict, for `/disks` (R2) |
| `unreadable_since` | int64 | First failed read (or forced read) since the last successful read; 0 otherwise |
| `transport` | string | `nvme`, `sata`, `sas`, `usb`, `other` (R5) |

Row columns kept: `first_seen_at` (start of the self-test grace when no pass was ever observed),
`last_seen_at` (= last successful read, FR-003), `retired`.

`SelfTestID` = `{hours int64, type int, code int}`.

Forced read due: asleep this run and `now - max(last_seen_at, forced_read_at) ≥
disks.force_read_after`. At most one forced read per disks run, serialised, on the due drive with
the oldest `max(last_seen_at, forced_read_at)`; the others wait for later runs (R4).

Upgrade (R5): a stored baseline whose drive now classifies as `usb` or `other` and whose serial is
not in `include_serials` is retired silently with a log line; it never opens "drive missing".

Validation: `selftest_ok_at` ≤ now (a future value from a clock step is clamped to now); counters
never decrease in the baseline except by a drive reset, which moves the baseline down silently
(001 rule).

### Drive status (new, derived; read by `/disks` and the summary)

`disks.Check.Status() []DriveStatus`, built from the baselines and the last run:

| Field | Source |
|---|---|
| Serial, Model, Bus, Transport | baseline |
| Node | `node` |
| Health | baseline `health` |
| LastReadAt | `last_seen_at` |
| AsleepSince, UnreadableSince | baseline |
| Temperature | last `temp.drive.<serial>` sample |
| SelfTestNewest, SelfTestPassAge | `selftest_newest`; `now - max(selftest_ok_at, first_seen_at)` |
| SelfTestReadable | `selftest_format == ok` |
| Wear | last `drive.<serial>.wear` sample (NVMe or SATA SSD) |
| Spare, SpareThreshold | last samples |

Mount points are not part of `DriveStatus`: the report layer adds them at render time with
`report.DriveMounts` over `sysfs.MountInfo()` and `sysfs.BlockTopology()` (`[]string`; empty ⇒ `-`,
read error ⇒ `?`).

State per drive:

```text
            read ok                       read finds standby
  Awake ◄──────────────── Asleep ◄────────────────────── Awake
    │                       │ max(last read, last forced read) older than force_read_after,
    │ read fails            │ and this run's single forced-read slot is free (oldest first)
    ▼                       ▼
 Unreadable ── read ok ──► Awake   forced read (forced_read_at := now) ── ok ──► Awake
                                    └─ fails ─► Unreadable (checker error, problems kept;
                                                no retry before force_read_after again)
```

### Counter set (new, R1)

`built-in ∪ disks.extra_counters ∪ disks.extra_counters_by_serial[serial]`, ATA ids 1–255.
Operator entries can only add; a built-in id listed again is accepted and has no effect.

### Self-test judgement (R3)

| Input | `disks/<serial>/selftest` | Other |
|---|---|---|
| Log section missing (ATA, NVMe) | no reading | checker error `disks/<serial>/selftest-log` |
| Format unsupported (always for SCSI/SAS) | no reading | status says not readable; no checker error |
| Newest finished failed | Bad | — |
| New failure not newest | (by age rule) | one Event `disks/<serial>/selftest-failure` |
| Age > `selftest_max_age` | Bad | — |
| Otherwise | Good | — |

## Temperatures (`pkg/health/check/temps`, `pkg/health/check/disks`)

### Temperature limit (new)

| Class | Key | Default °C | Overrides |
|---|---|---|---|
| NVMe drive | `limits.drive_nvme` | 60 | `limits_by_serial[serial]` |
| SATA/SAS drive | `limits.drive_sata` | 60 | `limits_by_serial[serial]` |
| CPU package | `limits.cpu` | 90 | — |
| GPU | `limits.gpu` | 85 | — |
| Listed board sensor | `board_sensors[].limit` | `limits.board` (75) | per entry |
| Unlisted board sensor | its own `temp<n>_crit` | — | — |

Broken when the reading is **at or above** the limit. `threshold` default 0. Plausible range
−40…150 °C; outside ⇒ Unknown.

### Sensor (hwmon adapter, amended)

`Sensor{Chip, Label, Device string; Celsius float64; Max, Crit *float64; Err error}` — `Device`
is the resolved hwmon device path (orders CPU packages, R12); `Err` set when the input exists but
cannot be read (`Celsius` is then meaningless). `temps.Sensor` has the same fields, so cmd keeps
converting one into the other.

### GPU (nvidia adapter, amended)

`GPU{Index, Name, Celsius *float64}` (nil for `[N/A]`), the same in `temps.GPU`;
`nvidia.ErrNoDevices` for "No devices were found", mapped by cmd to `temps.ErrNoGPUs`.

### Sensor state transitions (amended from 001)

```text
 present, readable   ─► State reading (Good/Bad)
 present, unreadable ─► Unknown (checker error), state kept, open problem kept
 absent, chip absent, condition open     ─► "gone" Event + Forget
 absent, chip absent, condition not open ─► Forget only (no message)
   (open is asked through temps.OpenState.IsOpen(key); cmd binds it late to Runner.IsOpen)
 core sensor (no longer judged)          ─► Forget once (Event only if it was open)
```

## Kernel (`pkg/health/check/kernel`)

### Kernel cursor (extends 001 KernelCursor)

| Field | Type | Notes |
|---|---|---|
| `boot_id` | string | 001 |
| `seq` | uint64 | 001 |
| `seen` | bool | 001 |
| `usec` | uint64 | kmsg time since boot of the record at `seq`; matches the journal's `_SOURCE_MONOTONIC_TIMESTAMP` (R7) |

Transition at start: stored `boot_id` ≠ current ⇒ read the journal tail of the stored boot after
`usec`, bounded by `health.Bound` at `health.MaxCheckTimeout` (20 s) and 200 000 records ⇒ report
⇒ cursor := `{current boot, 0, false, 0}`. Cut off by the deadline or the cap ⇒ report what was
read, checker error `kernel/boottail/incomplete`, cursor reset (one attempt, never retried); the cap reaches
the check as `kernel.ErrJournalCapped`. No
persistent journal (`kernel.ErrNoJournal`) ⇒ log once, cursor reset.

### Episode (amended)

Adds `unreported int` (records since the last message). "Still happening" fires on any `Run`
with `unreported > 0` and 6 h since the last message (R10). EDAC `ue` rises do not use episodes.

### Lost records

`lost uint64` in memory: `Σ (seq gap)` + inbox overflow since the last report; one checker error
`kernel/kmsg/lost` per run with `lost > 0` (R9).

### EDAC counters (amended)

`Counters{Controller, Correctable, Uncorrectable, Err}` — an `Err` controller keeps its stored
values and raises `kernel/<controller>/edac-unreadable` (R11).

## Delivery (`pkg/health/notify`)

### QueuedMessage (amended)

`{ID, CreatedAt, Channel, Text, Attempts, Kind, Check, Key}` — `Kind` ∈ `problem`, `resolved`,
`event`; rows from 001 read as `event`.

### Channel health (new, R17)

Per channel: `{FailingSince time?, Reported bool}`. kv `notify.channels` (JSON).

```text
 OK ── send fails / queued while offline ──► Failing(since)
 Failing ── telegram_failing_after passed, not Reported ──► email checker error; Reported = true
 Failing/Reported ── any successful send ──► OK
```

### SMS-sent problems (new, R18)

Set of condition keys whose PROBLEM went by SMS through the fallback. kv `notify.sms_problems`.
A RESOLVED for a key in the set also goes by SMS; the key leaves the set.

### Undelivered PROBLEM (new, R19)

Item `{Key, Text, Since, DroppedAt}`; kv `notify.undelivered` = `{Items []Item, Folded int}`
(JSON). Added for a dropped PROBLEM whose condition is tracked but not open, or no longer tracked
(R19).

- `Items` holds at most 50, oldest first; a PROBLEM listed while it is full increments `Folded`
  instead. Nothing is ever dropped to a log line only.
- The next message delivered to Telegram carries the prefix `Undelivered earlier:`, then as many
  item lines as fit the message limit, then, when `Folded > 0`, the counted line
  `… and N more undelivered PROBLEMs`, which is always shown (its space is reserved first).
- After that delivery succeeds, the items it showed are removed and `Folded` is reset; items
  whose lines did not fit stay for the next delivered message.

## Runner (`pkg/health/runner`)

### Maintenance lock observation (new, R16)

`{HeldSince time?, LastSeenHeld time?}`; kv `health.maintenance`. Continuity across restarts only
when `now - LastSeenHeld ≤ 5 min`. Checker error `health/maintenance/stuck` when
`now - HeldSince > general.maintenance_max_hold`.

### Snapshot (amended, R15)

`runner.Snapshot` gains `Paused []string`: the configured checks with `PauseInQuiet`, listed by
the `/status` quiet note. New `Runner.IsOpen(key) bool` (from `Engine.Condition`) answers
`temps.OpenState`.

### Store health (amended, R20)

`saveReported bool` in memory; the first successful checkpoint after it sends one recovery notice
and reverts the `health/store/write` rate marks.

## Service checks

### Freshness source (new, R26)

| Variant | Keys | Read result |
|---|---|---|
| File | `path`, `content: epoch \| mtime` | 001 |
| HTTP | `url`, `content: epoch`, `timeout`, `header` | fetched by `pkg/host/httpstamp` through `freshness.StampSource`; body parsed (pure) ⇒ Unix seconds; no answer, non-2xx, bad body ⇒ Unknown |

Both judged by `max_age` and `future_tolerance` (R25). Stamp kept for the summary in kv
`freshness.<name>.stamp` (Unix seconds) and `freshness.<name>.read_ok` (`0`/`1`).

### Command verdict order (R24)

start failure ⇒ checker error; timeout ⇒ `timeout_verdict`; exit ≠ 0 ⇒ Bad; `forbid_output`
match ⇒ Bad; `expect_output` no match ⇒ Bad; else Good.

## SQLite schema (`pkg/health/store`, `user_version = 2`)

```sql
-- migration 2
ALTER TABLE queue ADD COLUMN kind TEXT NOT NULL DEFAULT 'event';
ALTER TABLE queue ADD COLUMN check_name TEXT NOT NULL DEFAULT '';
ALTER TABLE queue ADD COLUMN key TEXT NOT NULL DEFAULT '';
```

`store.validate` rejects a queue `kind` outside `problem`, `resolved`, `event`. All other new
state lives in `baselines.data` and `kv` (see above). New kv keys: `notify.channels`,
`notify.sms_problems`, `notify.undelivered`, `health.maintenance`, `freshness.<name>.stamp`,
`freshness.<name>.read_ok`; the kernel kv value gains `cursor.usec` and `episodes.*.unreported`.

New samples: `drive.<serial>.nvme_err_log` (history only). `drive.<serial>.selftest_age_h` now
carries calendar hours.

## Validation rules (from requirements)

- `disks.spare_floor_percent` 0–100; `disks.force_read_after` ≥ 1 h; ATA ids in counter lists
  1–255; serial keys non-empty.
- `temperatures` limits and `limits_by_serial` values 0–150; `board_sensors[]` needs `chip`.
- `general.maintenance_max_hold` > 0; `general.telegram_failing_after` ≥ 1 m.
- `http.auth_refusal` ∈ {`down`, `checker_error`}; `header.placeholders` entries non-empty.
- `command.expect_output` / `forbid_output` compile as Go regular expressions;
  `timeout_verdict` ∈ {`broken`, `unreadable`}.
- `freshness`: exactly one of `path`, `url`; `url` requires `content: epoch`; `timeout` > 0 and
  ≤ 20 s; `future_tolerance` ≥ 0.
- `summary_group` matches `[a-z0-9-]+`.
- Message texts never include command output, header values or URLs (001 rule extended to the new
  sources).
