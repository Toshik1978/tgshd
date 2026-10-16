# Data Model: Host Health Monitoring

In-memory structures are authoritative; SQLite tables are their checkpoint and the history.
All times are Unix seconds (UTC) in storage.

## In-memory domain types (`pkg/health`)

### Reading
One judgement a check produces for one condition in one run.

| Field | Type | Notes |
|---|---|---|
| Key | string | Condition key, stable across runs: `<check>/<subject>[/<aspect>]`, e.g. `disks/<serial>/wear`, `containers/<name>`, `http/<name>` |
| Verdict | enum | `Good`, `Bad`, `Unknown` (unreadable) |
| Text | string | Human text for PROBLEM (`Bad`) or the error (`Unknown`); never contains secrets or raw event attributes |
| Mode | enum | Delivery override: `tlg`, `sms`, `tlg-or-sms` (else the check's mode) |
| Kind | enum | `State` (threshold rule) or `Event` (one message per occurrence: counter rise, drive announce, retire) |
| Since | time? | When the check itself saw the violation begin. A check that withholds its `Bad` reading until its own threshold passes sets it, so the PROBLEM states the real start of the streak. The engine uses it only when it first tracks the condition (it becomes `StreakStart`) and ignores a future time |

### Sample
| Field | Type | Notes |
|---|---|---|
| Metric | string | e.g. `temp.cpu.package`, `temp.drive.<serial>`, `drive.<serial>.wear`, `drive.<serial>.written_bytes`, `drive.<serial>.spare`, `drive.<serial>.selftest_age_h`, `ups.charge`, `ups.voltage`, `temp.gpu.<index>` |
| Value | float64 | |

### CheckResult
`{ Readings []Reading; Forget []string; Samples []Sample; Err error }` — a non-nil `Err` is reported as a checker
error. Readings returned alongside it (a partial failure) are still judged; conditions absent from
them are untouched. A run that failed outright returns no Readings and changes nothing.

`Forget` lists condition keys to clear silently once the readings are applied, as `/clear` does:
no message is sent and the alert log records the clear (kind `cleared`, text `sensor gone`). A check
uses it for a subject that is gone for good, e.g. `temps` for a vanished sensor. The runner honours
it only for the reporting check's own conditions (another check's key is ignored), and also when
`Err` is set.

### Condition (state)
| Field | Type | Rules |
|---|---|---|
| Key | string | PK |
| Check | string | owning check name |
| StreakStart | time? | set on first Bad of a streak; cleared on Good |
| BadFor | duration | accumulated per R1; reset on Good |
| LastReadingAt | time? | advanced only by Good/Bad |
| LastText | string | text of the last Bad reading |
| Open | bool | PROBLEM sent and not yet RESOLVED |
| OpenedAt | time? | |

**State transitions**

```text
          Bad (BadFor < threshold)          Bad (BadFor >= threshold) → emit PROBLEM
 Healthy ───────────────────────► Streak ─────────────────────────────────────► Open
    ▲  ◄──────── Good ───────────────┘                                            │
    └──────────────────────── Good → emit RESOLVED ◄──────────────────────────────┘
 Unknown in any state: no transition, LastReadingAt unchanged.
 /clear <key>: any state → Healthy, no message (alert record kind=cleared).
 Condition absent from a check's readings for a whole run while that check succeeded
   and the subject is gone: resolved once as "no longer exists" (containers) or via retire (drives).
 Sensor gone (temps): one "gone" Event reading, then the check lists the key in Forget and the
   runner clears the condition silently in the same run (alert record kind=cleared, no message).
```

### Baseline (per drive)
| Field | Type | Notes |
|---|---|---|
| Serial | string | PK |
| Model, Bus | string | display |
| Counters | map[string]int64 | ATA attrs 5, 187, 188, 197, 198, 199 raw; NVMe media_errors, num_err_log_entries — only those the drive reports |
| SelfTestLastSeenHours | int64 | power-on hours of newest finished self-test judged |
| SelfTestOKAt | time? | last passed self-test seen |
| FirstSeenAt, LastSeenAt | time | |
| Retired | bool | set by `/retire`; a retired serial that reappears is announced as new |

### KernelCursor
`{ BootID string; Seq uint64 }` plus per-class `{ FirstAt, LastAt, LastStillAt time; Count int }`.

### ProbeState
`{ SentAt time?; Token string; LastOKAt time?; FirstMissingAt time? }`

### RateMark
`{ Key string; Channel string; LastSentAt time }` — checker errors (24 h), kernel "still happening" (6 h).

### QueuedMessage
`{ ID; CreatedAt; Channel (tlg|sms|email); Text; Attempts }`

### Heartbeat (file, not DB)
`ts=<epoch> planned_until=<epoch>` — see [contracts/heartbeat.md](contracts/heartbeat.md).

## SQLite schema (`pkg/health/store`, `user_version = 1`)

```sql
CREATE TABLE conditions (
  key TEXT PRIMARY KEY, check_name TEXT NOT NULL,
  streak_start INTEGER, bad_for_s INTEGER NOT NULL DEFAULT 0,
  last_reading_at INTEGER, last_text TEXT NOT NULL DEFAULT '',
  open INTEGER NOT NULL DEFAULT 0, opened_at INTEGER
);
CREATE TABLE baselines (
  serial TEXT PRIMARY KEY, model TEXT, bus TEXT,
  data TEXT NOT NULL,              -- JSON: counters, self-test fields
  first_seen_at INTEGER NOT NULL, last_seen_at INTEGER NOT NULL,
  retired INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE kv (k TEXT PRIMARY KEY, v TEXT NOT NULL);           -- kernel cursor, probe state, rate marks, first-run flag
CREATE TABLE alerts (
  id INTEGER PRIMARY KEY, at INTEGER NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('problem','resolved','event','fix','retired','cleared','checker_error')),
  check_name TEXT NOT NULL, key TEXT NOT NULL, text TEXT NOT NULL
);
CREATE INDEX alerts_at ON alerts(at);
CREATE INDEX alerts_check_at ON alerts(check_name, at);
CREATE TABLE samples (at INTEGER NOT NULL, metric TEXT NOT NULL, value REAL NOT NULL);
CREATE INDEX samples_metric_at ON samples(metric, at);
CREATE TABLE samples_daily (
  day INTEGER NOT NULL, metric TEXT NOT NULL,  -- day = epoch of 00:00 local
  min REAL NOT NULL, avg REAL NOT NULL, max REAL NOT NULL, n INTEGER NOT NULL,
  PRIMARY KEY (day, metric)
);
CREATE TABLE queue (
  id INTEGER PRIMARY KEY, created_at INTEGER NOT NULL,
  channel TEXT NOT NULL CHECK (channel IN ('tlg','sms','email')),
  text TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0
);
```

## Retention (daily job)

| Data | Rule |
|---|---|
| `alerts` | delete `at < now - 365 d` |
| `samples` | for days older than 90 d: aggregate into `samples_daily` (upsert), then delete those rows |
| `samples_daily` | kept (≈ metrics × days; a few MB per year) |
| `queue` | delete entries older than 7 d (logged as dropped) |

## Validation rules (from requirements)

- Condition keys never contain secrets; subjects are names from config, container names, drive
  serials or sensor labels.
- Thresholds ≥ 0; intervals ≥ 30 s (the tick); max ages > 0; status ranges within 100–599.
- `http` and `command` timeouts > 0 and ≤ 20 s (`health.MaxCheckTimeout`, well inside the 40 s
  run budget; command default 20 s, http default 15 s).
- An entry error never repeats a URL (its userinfo or query may be secret).
- `planned_until` window 0–86 400 s (FR-017).
- Message text ≤ 4 096 characters after the host prefix (truncate with `…`).
