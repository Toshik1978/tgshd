# Research: Host Health Monitoring

Decisions taken while planning. Each entry: **Decision**, **Rationale**, **Alternatives considered**.
All findings were validated by a throwaway spike on a real Linux host (static `CGO_ENABLED=0`
build); nothing host-specific is recorded here.

## R1. Alert engine time accounting

- **Decision**: Each condition keeps `streak_start` (first bad reading of the current streak),
  `bad_for` (accumulated bad time with readings), `last_reading_at` and `open` (PROBLEM sent).
  On every judged reading the engine adds `min(now - last_reading_at, gap_cap)` to `bad_for`,
  where `gap_cap` is 1.5 × the check's interval. Time across a daemon outage, a quiet window or
  an unreadable stretch therefore never counts. PROBLEM fires when `bad_for >= threshold`
  (threshold 0 ⇒ the first bad reading). A good reading resets the streak and, if `open`, emits
  RESOLVED. Unreadable readings change nothing but `last_reading_at` is **not** advanced, so the
  next judged reading after a gap is capped.
- **Rationale**: Reproduces "only time with readings counts" (FR-004) without wall-clock
  arithmetic on excluded windows, and is a pure function of (state, reading, now) — trivially
  table-testable. A clock jump is bounded by `gap_cap` too.
- **Alternatives considered**: subtracting recorded excluded windows from wall time (the shell
  approach; needs a window ledger); counting consecutive bad samples (breaks when intervals change).

## R2. Scheduling model

- **Decision**: One `app.Worker` named `health` with a 30 s tick (gocron, singleton mode, as all
  workers today). Each tick the runner: writes the heartbeat; returns early inside a quiet window
  (maintenance lock or boot grace); runs the `network` check first if due; then runs every other
  due check concurrently (bounded, default 4), skipping `needs_internet` checks while the outside
  is known down and checks whose `depends_on` target is currently bad; feeds readings to the
  engine; dispatches events; flushes queued messages; checkpoints state. Daily jobs (retention,
  roll-up, write snapshot) and the weekly summary are "due" computations inside the same tick.
- **Rationale**: Fits the existing `Worker`/gocron pattern, keeps ordering rules (network first,
  dependencies) in one place, and makes SC-006 (heartbeat ≤ cycle + 1 min) hold by construction.
- **Alternatives considered**: one gocron job per check (loses network-first ordering and
  dependency skips); gocron cron jobs for the summary (the `Worker` interface is duration-only;
  a weekday/time "due" check is ~15 lines).

## R3. SMART data source

- **Decision**: `smartctl --json=c -a /dev/<node>` per drive, decoded into Go structs. The
  exit status is a bitmask: bits 0–1 (command line / device open failed) ⇒ unreadable;
  bit 3 (disk failing) and `smart_status.passed=false` ⇒ health bad; other bits are informational
  (the JSON carries the details). One exec per drive per hardware cycle (default 15 min).
  Parsing lives in the check package and is tested with anonymised JSON fixtures; the adapter
  only runs the command.
- **Rationale**: Maintainer decision after comparing options. One call yields health, ATA
  attributes, NVMe health log, self-test logs for both buses and temperature; smartmontools is
  the reference implementation, already required by `smartd`, handles USB bridges, SAS and its
  per-model quirks database; `json_format_version` keeps output stable.
- **Alternatives considered**: `anatol/smart.go` (pure Go; validated in the spike but lacks the
  NVMe self-test log and the quirks database; single maintainer); own ioctl implementation
  (~500 lines, SATA/NVMe only).

## R4. Drive discovery

- **Decision**: Enumerate `/sys/block/*`, keep devices with a `device` link (physical), skip
  `loop*`, `ram*`, `zram*`, `dm-*`, `md*`, `sr*`, `removable=1`, and `size=0` (no media). Identity
  is the serial reported by smartctl; the node name is display-only.
- **Rationale**: Standard library only; matches "skip removable / no-media / virtual" (FR-020).
- **Alternatives considered**: `jaypipes/ghw` (validated in the spike, but adds a dependency for
  ~40 lines of sysfs reading); `lsblk -J` exec.

## R5. Temperatures — gopsutil dropped

- **Decision**: Drive temperatures come from the SMART JSON and are judged by the disks check
  itself against `temperatures.limits.drive_nvme` / `drive_sata` (per drive, keyed by serial, so
  the "two identical NVMe sensor names" problem disappears and no cross-check ordering is needed). CPU, chipset and board temperatures are
  read from `/sys/class/hwmon/hwmon*/{name,temp*_input,temp*_label,temp*_max,temp*_crit}` with
  the standard library. GPU via `nvidia-smi --query-gpu=index,name,temperature.gpu --format=csv,noheader,nounits`
  when the binary exists.
- **Rationale**: After moving drive temperatures to SMART, gopsutil's only remaining use would be
  a hwmon walk the spike showed loses the device mapping anyway. Dropping it removes one of the
  four approved dependencies (net reduction; no approval needed to *not* add one).
- **Alternatives considered**: `shirou/gopsutil/v4` sensors (approved, works; redundant here);
  NVIDIA `go-nvml` (requires cgo — rejected by the build constraint).

## R6. Kernel hardware errors

- **Decision**: A long-lived reader on `/dev/kmsg` (root) parses records
  `prio,seq,usec,flags;message`, classifies messages with an anchored pattern table per class
  (storage I/O, NVMe, ATA, machine check, thermal throttle), and buffers per-class counts and last
  sample line between ticks. The last processed `seq` and the boot id
  (`/proc/sys/kernel/random/boot_id`) are persisted, so a daemon restart within the same boot
  neither re-reports nor misses records. EDAC: `/sys/devices/system/edac/mc/mc*/{ce_count,ue_count}`
  as rising counters; absent directory ⇒ no reading (not an error).
- **Rationale**: Pure Go, no `dmesg` exec, no journal (journal readers in Go need cgo).
- **Alternatives considered**: `coreos/go-systemd/sdjournal` (cgo); polling `dmesg` (text scraping).

## R7. Containers

- **Decision**: `github.com/moby/moby/client` (`ContainerList` + `ContainerInspect`) each
  service cycle. Watched = restart policy `always` or `unless-stopped`, minus the exclude list.
  Broken when not running, `restarting`, health `unhealthy`, or `RestartCount`/`StartedAt`
  changed since the previous observation while that observation saw it up (crash loop).
  Only name, action and state are ever put in messages or logs (FR-029).
- **Rationale**: Inspect deltas catch every restart between cycles without a long-lived event
  stream and its reconnect logic; equivalent detection, less code.
- **Alternatives considered**: the Docker event stream (validated in the spike; rejected for the
  reconnect/backfill complexity and because event attributes carry raw health-check command lines);
  exec of the `docker` CLI (text scraping).

## R8. HTTP checks and DNS-over-HTTPS

- **Decision**: `net/http` with a per-check timeout, no redirects followed beyond 5, status range
  check. Optional `doh` resolves the host via RFC 8484 (POST `application/dns-message`) using
  `golang.org/x/net/dns/dnsmessage` to build/parse the DNS message; the transport's `DialContext`
  then dials the resolved address while TLS still verifies the original host name. Optional
  header value is read from an env var named in config, never logged. `golang.org/x/net` as a
  direct dependency was approved by the maintainer on 2026-10-16.
- **Rationale**: Exercises the path "from the internet back through a tunnel" without touching
  LAN DNS. `golang.org/x/net` is already in the module graph (indirect) and maintained by the Go
  team; it becomes a direct dependency (approved).
- **Alternatives considered**: DoH JSON API (non-standard, provider-specific); hand-rolled DNS
  wire encoding (~150 lines of protocol code).

## R9. Persistence

- **Decision**: `modernc.org/sqlite` (pure Go), one database file under the state directory
  (default `/var/lib/tgshd/health.db`), WAL mode, `busy_timeout`, a single connection. Schema
  versioned with `PRAGMA user_version` and forward-only migrations in code. In-memory state is
  authoritative; at the end of each tick the runner writes the dirty set (conditions, baselines,
  kv, queued messages, new alert records and samples) in one transaction. On write failure the
  store marks itself degraded, reports one checker error, keeps accumulating in memory, and
  retries every tick; the first successful write flushes everything (FR-013).
- **Rationale**: Validated in the spike (static build, SQLite 3.53, WAL). Queryable history for
  `/history`, `/temp`, `/disks` and the weekly summary.
- **Alternatives considered**: JSON checkpoint file (no history queries); `etcd-io/bbolt` (no
  query layer); `mattn/go-sqlite3` (cgo).

## R10. Configuration

- **Decision**: One YAML file (path in `BOT_HEALTH_CONFIG`; its presence enables health
  monitoring — the env-gated pattern). Decoded with `go.yaml.in/yaml/v3` using `KnownFields(true)`;
  every entry validated independently; an invalid entry becomes a checker error naming file,
  entry and reason, and is skipped (FR-043). Secrets are never in the file: a check names the env
  var that holds them (`header_env`). Durations use Go syntax (`15m`, `8d` is accepted as days by
  a small wrapper).
- **Rationale**: Readable for lists of checks; strict decoding catches typos.
- **Alternatives considered**: env vars only (do not fit lists of structured checks); TOML
  (another dependency, no advantage); JSON (stdlib, but unfriendly to hand-edit, no comments).

## R11. Notifications and delivery

- **Decision**: A notifier turns engine events into messages with a host-name prefix (FR-010;
  `os.Hostname()`, overridable by `BOT_HOST_NAME`), HTML-escaped for Telegram's HTML mode. Modes:
  `tlg`, `sms`, `tlg-or-sms`. SMS goes through the existing gammu outbox backend; if gammu is not
  configured, `sms` degrades to `tlg` and a checker error is raised once. Failed Telegram sends
  are queued (persisted) and retried each tick; while the outside is known down they are queued
  silently (FR-009).
- **Rationale**: Reuses the existing publisher and SMS backend; one place for wording and routing.
- **Alternatives considered**: per-check senders (duplicated routing rules).

## R12. Checker errors and email

- **Decision**: Checker errors are keyed (`check/subject/kind`), always logged, and sent to
  Telegram and, when `BOT_SMTP_ADDR` is set, by email via stdlib `net/smtp` (STARTTLS when the
  server offers it, optional PLAIN auth). Each key at most once per 24 h per channel (FR-045).
- **Rationale**: Replaces cron mail (the old carrier of checker errors) without a dependency.
  `net/smtp` is frozen but stable and sufficient for a relay submission.
- **Alternatives considered**: a third-party mail library (unneeded); journal only (invisible).

## R13. Quiet windows, single instance, heartbeat

- **Decision**:
  - Maintenance lock: open the configured path read-only and `syscall.Flock(LOCK_SH|LOCK_NB)`,
    release immediately; `EWOULDBLOCK` ⇒ held exclusively ⇒ quiet; missing file ⇒ not held.
  - Boot grace: `/proc/uptime` < grace ⇒ quiet.
  - Single instance: exclusive `flock` on `<state>/tgshd.lock` held for the daemon's life; a
    second daemon starts with health monitoring disabled and a logged error.
  - Heartbeat: `ts=<epoch> planned_until=<epoch>` written by temp file + rename under an
    exclusive `flock` on a sidecar `<path>.lock`; the daemon re-reads `planned_until` under the
    same lock before rewriting, so the CLI and the daemon never lose each other's update.
- **Rationale**: Same contract the external watchdog already understands; standard library only.

## R14. Incoming SMS from the gammu SQL inbox

- **Decision**: Poll `inbox WHERE "Processed" = false` every 15 s; group multipart rows by
  sender + UDH concatenation reference; forward when all parts are present, or after 10 minutes
  with what arrived; then `UPDATE inbox SET "Processed" = true` for those IDs. Probe replies are
  recognised before forwarding. Requires the operator to remove `runonreceive` from the gammu
  configuration (documented in quickstart), otherwise messages are delivered twice.
- **Rationale**: tgshd already owns the gammu database connection (outbox); keeps the modem probe
  loop in one process.
- **Alternatives considered**: keep the `runonreceive` hook writing a file tgshd reads
  (cross-process contract, the thing this feature removes).

## R15. Telegram access filter

- **Decision**: The consumer drops any update whose `Chat.ID` is not in the allowed set
  (`TELEGRAM_CHAT_ID` ∪ `TELEGRAM_ALLOWED_IDS`, comma-separated) before dispatch, logging the
  chat and sender IDs at warn level, replying nothing. Applies to all handlers, existing and new.
- **Rationale**: Closes the existing gap (any user could run commands, including the root script
  hook). Chat-based rather than sender-based so a private chat with an allowed user ID and the
  configured group both work, and a stranger's group never does.
- **Alternatives considered**: per-handler checks (the current `/sms` approach; easy to forget);
  sender-based (an allowed user could drive the bot from any group).

## R16. One-shot mode and CLI

- **Decision**: `cmd/main.go` dispatches on `os.Args[1]` before fx: `check --once` builds the
  configured checks with real adapters, a printing notifier and a read-only view of the store
  (baselines shown when present), runs each check once and prints reading + verdict, exit 0 when
  it ran (even with bad verdicts), 1 on a configuration or setup error. `heartbeat
  --planned-reboot <seconds>` updates the heartbeat file and exits. No argument ⇒ the daemon.
- **Rationale**: Safe live verification (SC-001) and the reboot-script hook (FR-017) without a
  control channel into the daemon (clarification Q2).
