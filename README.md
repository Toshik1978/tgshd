[![Lint, Test & Build](https://github.com/Toshik1978/tgshd/actions/workflows/ci.yml/badge.svg)](https://github.com/Toshik1978/tgshd/actions/workflows/ci.yml)
[![Tests](https://img.shields.io/endpoint?url=https%3A%2F%2Fgist.githubusercontent.com%2FToshik1978%2F302dd7bfb9434434a7b129aba6e3a723%2Fraw%2Ftests.json)](https://github.com/Toshik1978/tgshd/actions/workflows/ci.yml)
[![Coverage](https://img.shields.io/endpoint?url=https%3A%2F%2Fgist.githubusercontent.com%2FToshik1978%2F302dd7bfb9434434a7b129aba6e3a723%2Fraw%2Fcoverage.json)](https://github.com/Toshik1978/tgshd/actions/workflows/ci.yml)

# tgshd — Telegram Shell Daemon

A small Go daemon for Ubuntu servers that turns a Telegram chat into a control
surface for the box. It processes commands you send over Telegram and runs
periodic background checks — a lightweight toolbox for remote server
monitoring and control.

## Features

**Telegram commands**

| Command | What it does | Enabled when |
| :--- | :--- | :--- |
| `/ping` | Pings the configured hosts and reports which are up | `BOT_PING_HOSTS` is set |
| `/power` | Reports UPS input voltage via a NUT server | NUT is configured |
| `/speed` | Runs an internet download/upload speed test | always |
| `/5g` `/4g` `/3g` | Switches the ZTE MC888 modem's network bearer | ZTE modem is configured |
| `/sms <phone> <message>` | Sends an SMS through gammu-smsd | gammu is configured |
| `/status` | Lists open health problems, or confirms all checks are OK | `BOT_HEALTH_CONFIG` is set |
| `/clear <condition key>` | Clears an open health condition without sending RESOLVED | `BOT_HEALTH_CONFIG` is set |
| `/retire <serial>` | Retires a removed drive (full serial or its last 4+ characters) and clears its conditions | `BOT_HEALTH_CONFIG` is set |
| `/history [check] [24h\|7d\|30d]` | Lists past health alerts, newest first (default 7d) | `BOT_HEALTH_CONFIG` is set |
| `/temp` | Shows each temperature sensor now and its 7-day min/avg/max | `BOT_HEALTH_CONFIG` is set |
| `/disks` | Shows each drive's health, wear and temperature (with 7-day min/avg/max), spare, data written in 7 days and self-test age | `BOT_HEALTH_CONFIG` is set |
| *custom* | Names/executes commands from your own script | `TELEGRAM_UNKNOWN_SCRIPT` is set |

**Background workers**

- **Incoming SMS forwarding** — polls the ZTE MC888 modem for new SMS and
  forwards them to your Telegram chat (enabled when the ZTE modem is configured).
  With gammu configured, SMS received by gammu-smsd are forwarded the same way
  (see [Incoming SMS through gammu](#incoming-sms-through-gammu)).

- **Host health monitoring** — scheduled checks with PROBLEM/RESOLVED alerts that
  survive restarts (see [Host health monitoring](#host-health-monitoring)).

**Access control**

Only `TELEGRAM_CHAT_ID` and the optional `TELEGRAM_ALLOWED_IDS` may command the
bot; updates from any other chat are ignored and logged. `/sms` stays limited
to `TELEGRAM_CHAT_ID`.

Every hardware/service-backed feature is **optional**: if it isn't configured,
that feature is simply inactive and the daemon still runs.

## Requirements

- **Go 1.27+** (the binary compiles with `CGO_ENABLED=0`)
- **[go-task](https://taskfile.dev)** — all automation is driven through `Taskfile.yml`
- Optional, per feature: a [NUT](https://networkupstools.org/) server (power),
  a ZTE MC888 modem (network switching + incoming SMS), and a
  [gammu-smsd](https://wammu.eu/smsd/) instance with a **PostgreSQL** backend
  (outgoing and incoming SMS, modem probe).
- Host health monitoring (`BOT_HEALTH_CONFIG`), hardware checks: the `disks`
  check needs **[smartmontools](https://www.smartmontools.org/)** (`smartctl`
  7.0+ for JSON output) on `PATH`; GPU temperatures use `nvidia-smi` when it is
  installed (optional, absent means no GPUs). `disks` reads every drive with
  `smartctl -n standby`, so a spun-down drive is skipped that run rather than
  woken (see the forced daily read below). `disks` and `kernel` read
  `/dev/*` and `/dev/kmsg`, so run the daemon as root (the systemd unit below).
  `journalctl` (systemd) is an optional host tool: after a reboot the `kernel`
  check uses it to read the previous boot's kernel log tail from the persistent
  journal. Without it, or without a persistent journal, that tail is skipped
  and a log line says so.
  Hardware checks are not supported in the Docker image (distroless, nonroot,
  no `smartctl`/`nvidia-smi`).
- Health monitoring, service checks: `containers`, the `container` option of an
  `http` entry and a `command` fix with `restart_container` talk to the Docker
  Engine API through `/var/run/docker.sock` (or `DOCKER_HOST`), so the daemon
  needs read/write access to that socket (root, or the `docker` group). Without
  Docker the daemon still starts; only those Docker-backed checks fail their
  runs with a checker error. `command` probes and fixes run without a shell;
  every fix is logged, and a reported fix (`report: alert`) is pushed on the
  entry's `mode` (`tlg`, `sms` or `tlg-or-sms`).
  `tgshd check --once` runs the probes but never runs a fix: it prints
  `WOULD FIX <key>: …` lines instead.

## Configuration

Configuration is read from the environment (a local `.env` is auto-loaded; see
`.env.dist` for a template).

| Variable | Description |
| :--- | :--- |
| `APP_ENV` | `development` (pretty logs) or `production` |
| `TELEGRAM_TOKEN` | Telegram bot token |
| `TELEGRAM_CHAT_ID` | Operator chat/user ID — reply target and the only sender allowed to run `/sms` |
| `TELEGRAM_ALLOWED_IDS` | Optional comma-separated extra chat IDs allowed to command the bot in addition to `TELEGRAM_CHAT_ID`; updates from any other chat are ignored and logged |
| `TELEGRAM_UNKNOWN_SCRIPT` | Path to a script that lists/executes custom commands |
| `BOT_PING_HOSTS` | Comma-separated hosts for `/ping` |
| `BOT_NUT_IP` | NUT server address |
| `BOT_NUT_NAME` | UPS name in NUT |
| `BOT_NUT_USER` / `BOT_NUT_PASS` | NUT credentials |
| `BOT_NUT_WARN` / `BOT_NUT_ERR` | Voltage warning/error thresholds |
| `BOT_ZTE_HOST` | ZTE MC888 modem address — enables the modem features |
| `BOT_ZTE_PASS` | ZTE modem admin password |
| `BOT_GAMMU_DSN` | PostgreSQL DSN of the gammu-smsd database — enables `/sms`, incoming SMS forwarding from its inbox and the `modem_probe` check |
| `BOT_HEALTH_CONFIG` | Path to the health YAML — enables host health monitoring (unset: off) |
| `BOT_HOST_NAME` | Prefix for every health message (default: the OS host name) |
| `BOT_SMTP_ADDR` | `host:port` of an SMTP relay — checker errors are also sent by email |
| `BOT_SMTP_FROM` / `BOT_SMTP_TO` | Envelope and header addresses; required with `BOT_SMTP_ADDR` |
| `BOT_SMTP_USER` / `BOT_SMTP_PASS` | Optional SMTP PLAIN authentication |
| `BOT_SMS_PHONE` | Operator phone number for SMS alerts; required for the `sms` and `tlg-or-sms` modes (needs gammu) |

## Host health monitoring

tgshd can watch the host it runs on and push alerts to Telegram (and, per
check, by SMS). Alerts are state based: `PROBLEM` when a condition stays bad for
longer than its threshold, `RESOLVED` when it recovers, and the state survives
restarts. It is **off** unless `BOT_HEALTH_CONFIG` points at a health YAML file;
start from [`config/health.example.yaml`](config/health.example.yaml), which
shows every key with a comment.

```bash
sudo install -d /usr/local/etc
sudo install -m 0644 config/health.example.yaml /usr/local/etc/tgshd-health.yaml
# then, in /usr/local/etc/tgshd.conf:
#   BOT_HEALTH_CONFIG=/usr/local/etc/tgshd-health.yaml
sudo systemctl restart tgshd
```

### Configuration file

The file is strict YAML: an unknown key is an error. An invalid entry is
reported as a checker error and skipped; a file that cannot be parsed disables
health monitoring. Every top-level section is optional: an omitted section
takes its defaults, and a check whose section is absent does not run.
Durations use Go syntax (`30s`, `15m`, `6h`) plus `d` for days (`8d`). Checker
errors (problems of the monitor itself, such as an invalid entry or an
unreadable sensor) go to Telegram, and are also emailed when `BOT_SMTP_ADDR`
is set.

**`general`** (daemon-wide):

| Key | Default | Meaning |
| :--- | :--- | :--- |
| `state_dir` | `/var/lib/tgshd` | State database and instance lock |
| `heartbeat_file` | `/run/tgshd/heartbeat` | Written every 30 s for an external watchdog |
| `maintenance_lock` | none | A file whose exclusive lock opens a maintenance window; none means no maintenance windows |
| `maintenance_max_hold` | `6h` | A maintenance lock held longer raises a checker error (the window stays) |
| `boot_grace` | `10m` | Quiet window after the host boots |
| `concurrency` | `4` | Checks run at the same time |
| `telegram_failing_after` | `15m` | Telegram failing this long sends one email notice |

**Keys every check accepts** (`modem_probe` takes none of them):

| Key | Default | Meaning |
| :--- | :--- | :--- |
| `interval` | per check, below | How often the check runs |
| `threshold` | per check, below | How long a condition must stay bad before its PROBLEM is sent; `0` sends it at once |
| `mode` | `tlg` | `tlg`, `sms` or `tlg-or-sms` (SMS needs gammu and `BOT_SMS_PHONE`) |
| `needs_internet` | `false` (`true` for an `http` entry with `doh`) | Skip the check while the `network` check finds the internet down or has not read it yet |
| `pause_in_quiet_windows` | `true`; `false` for `disks`, `temperatures`, `kernel` | Hold the check back during quiet windows |
| `summary_group` | none | Checks naming the same group (`[a-z0-9-]+`) share one line of the weekly summary; no effect on `disks`, `temperatures` and `kernel` |

### Checks

| Section | Watches | Interval / threshold | Keys of its own |
| :--- | :--- | :--- | :--- |
| `network` | Default route, gateway and internet reachability, as one condition; evaluated first every cycle | `5m` / its interval | `internet_targets` (any one answering means the internet is up). Mode is `sms`: the PROBLEM goes by SMS, the RESOLVED by `tlg-or-sms` |
| `ups` | NUT power status, using the `BOT_NUT_*` settings | `5m` / `5m` | — |
| `disks` | SMART health of NVMe, SATA and SAS drives (smartmontools) | `15m` / `0` | `wear_percent` (`80`), `selftest_max_age` (`40d` since a passed self-test was last seen), `spare_floor_percent` (`20`, NVMe), `force_read_after` (`24h`, at least `1h`), `include_serials` (USB and other hot-pluggable drives are skipped unless listed), `exclude_serials`, `extra_counters` and `extra_counters_by_serial` (extra ATA attribute ids, 1–255) |
| `temperatures` | One package sensor per CPU, GPUs (nvidia-smi), listed board sensors; drive limits are judged by `disks` | `15m` / `0` | `limits` in °C, a PROBLEM at or above: `cpu: 90`, `drive_nvme: 60`, `drive_sata: 60` (also SAS), `gpu: 85`, `board: 75`; `limits_by_serial` (per drive); `board_sensors` (`{chip, label, limit}`, `*` globs; only listed board sensors are judged) |
| `kernel` | EDAC memory error counters and the kernel log (read continuously) | `15m` / `0` | — |
| `memory` | Available memory, swap, memory pressure, service growth and containers without a memory limit; its `guard` restarts a runaway service (see [Memory protection](#memory-protection)) | `1m` / `5m` | `warn_available_percent` (`15`), `warn_swap_percent` (`80`), `warn_pressure_percent` (`10`), `growth_window` (`1h`), `growth_warn` (`4GB`), `audit_exclude`, `guard` |
| `containers` | Docker container health | `5m` / `15m` | `exclude` (globs on the container name) |
| `http` (list) | One endpoint per entry | `10m` / `10m` | `name`, `url`, `timeout` (`15s`, at most `20s`), `status` (`[200, 399]`), `connect_to` or `doh`, `header`, `follow_redirects` (`false`: a redirect is judged by its own status), `auth_refusal` (`down` or `checker_error`: what a 401/403 means), `depends_on`, `container` (also require this container healthy) |
| `freshness` (list) | Age of a status stamp: a file's mtime or a Unix time in a file or at a URL | `15m` / `0` | `name`, `path` or `url` (exactly one), `content` (`epoch` or `mtime`), `max_age`, `future_tolerance` (`1h`), and for `url`: `timeout` (`15s`, at most `20s`) and `header` |
| `command` (list) | A probe command you provide, with an optional fix | `5m` / `0` | `name`, `run` (argv, no shell), `timeout` (`20s`, at most `20s`), `expect_output` and `forbid_output` (Go RE2), `timeout_verdict` (`broken` or `unreadable`), `fix`, `report` |
| `modem_probe` | A weekly SMS round trip through the modem (needs `BOT_GAMMU_DSN`) | weekly | `phone_env`, `weekday` (`sunday`), `time` (`11:02`), `reply_within` (`30m`), `max_silence` (`8d`, longer than a week plus `reply_within`) |

A `summary` section (`weekday`, default `monday`; `time`, default `09:00`)
enables the weekly summary.

**`header`** (`http` and `freshness`): `{name, value_env, prefix,
placeholders}`. The value is read from the environment variable `value_env`
and sent after `prefix`; a value containing one of the `placeholders`
(case-insensitive) means the secret was never set and is reported as a checker
error.

**`depends_on`** (`http`): a condition `kind/name`, such as
`network/connectivity`, `command/port-guard` or another `http/<name>`; the
check is skipped while that condition is bad. An entry is rejected at load
when its `depends_on` names the entry itself, forms a cycle, names a condition
no configured check raises, or names a `disks` or `temperatures` condition
(those are per drive or sensor and have no `kind/name` key).
`network/router` and `network/internet` are accepted as aliases of
`network/connectivity`.

**`fix`** (`command`): `after` (consecutive failures, default `2`), either
`restart_container` or `run`, `max_per_hour` (`3`) and `grace_after_start`
(`2m`). `report: alert` (the default) sends each fix as a message;
`report: journal` only logs it.

**Timeouts.** The checks of one 30 s cycle share a run budget of 40 s. That is
why `http`, `freshness` and `command` timeouts are at most `20s`: a hung probe
is judged (a PROBLEM, and a strike towards a `command` fix) instead of being
abandoned.

### Behaviour

**Quiet windows.** A maintenance window (while `general.maintenance_lock` is
held) or the boot grace pauses every check except the hardware ones: `disks`,
`temperatures` and `kernel` keep running, because a failing drive matters most
during a reboot. Set `pause_in_quiet_windows: true` on one of them to pause it
as well, or `false` on any other check except `modem_probe` to keep it running.
Setting it `false` on a `command` check that has a fix lets the fix run during
maintenance windows. A check with `needs_internet: true` runs only once the
`network` check has read the network, so while `network` pauses (its default)
a maintenance lock held from boot keeps such a check waiting until the lock is
released.

**Delivery.** While Telegram keeps failing, each queued PROBLEM also goes by
SMS (needs gammu and `BOT_SMS_PHONE`), and the RESOLVED of a PROBLEM sent by SMS
goes by SMS too. The PROBLEM's Telegram copy stays queued and is delivered to
Telegram later. When Telegram has been failing for longer than
`general.telegram_failing_after`, one email names the failing channel and since
when (needs `BOT_SMTP_ADDR`; without it the failure is only logged). A PROBLEM
that leaves the retry queue undelivered is never dropped silently: its
condition is reopened so it is raised again, or it is listed under
`Undelivered earlier:` in the next delivered message (up to 50 listed, the rest
counted in a line that is always shown).

**Sleeping drives.** `disks` never wakes a drive on a routine read. A drive
whose last successful read is older than `disks.force_read_after` is read even
if that wakes it, at most one such drive per run.

**Telegram commands.** `/status` lists open problems (or `All checks OK`) and
notes a degraded store or quiet window; `/clear <condition key>` drops an open
condition without sending RESOLVED; `/retire <serial>` retires a drive that was
removed (the full serial or its last 4+ characters; an unknown or ambiguous
value lists the known drives) and clears its conditions.
`/history [check] [24h|7d|30d]` lists the alert log newest first (a check name
or condition key prefix filters it; the default period is 7d), `/temp` shows
each sensor's current value next to its 7-day min/avg/max, and `/disks` shows
each drive by bus, model and the last 4 serial characters with its health (OK,
FAILING or missing), wear and temperature (each followed by its 7-day
min/avg/max, e.g. `temp 41°C (7d 35/38/44)`), spare (NVMe), data written in the
last 7 days and the age of its newest self-test. These three read the state
database; while the daemon runs from memory they reply that history is
unavailable. All commands are accepted only from `TELEGRAM_CHAT_ID` and
`TELEGRAM_ALLOWED_IDS`.

**Weekly summary.** With a `summary` section the daemon pushes one message
(host local time), prefixed with the host name, listing each drive (health,
wear, data written this week), a services block (one line per configured check
other than `disks`, `temperatures` and `kernel`, or per `summary_group`: OK or
its open problems, with freshness ages and the modem probe state) and the open
problems. It fits one Telegram message and is cut with `…` when longer; a
section that cannot be built reads "unavailable".

**Retention.** Once a day the daemon prunes the state database: alerts after
365 days, raw samples after 90 days (rolled up into daily min/avg/max that
`/temp` and `/disks` keep using) and undelivered messages after 7 days (each
dropped one is logged).

### CLI subcommands

| Invocation | What it does |
| :--- | :--- |
| `tgshd` | Runs the daemon; health monitoring runs when `BOT_HEALTH_CONFIG` is set |
| `tgshd check --once` | Runs every configured check once with the real adapters and prints `<VERDICT> <key>: <text>` per reading plus a summary line; sends and writes nothing. A sleeping drive is never woken and prints `ASLEEP disks/<serial>: last read <age> ago` (or `never read`); each distinct checker error prints as `ERROR <check>/<subject>/<kind>: <text>`; the previous boot's kernel log is not read. If the state database is at an older schema version (the daemon has not been restarted since the upgrade) it runs without stored state and says so. Exit 0 ran, 1 config/setup error |
| `tgshd heartbeat --planned-reboot <seconds>` | Sets `planned_until` in the heartbeat file so an external watchdog tolerates a planned reboot; `0` clears it. Runs no checks. Exit 0 ok, 1 bad argument or write error |

`sudo` drops the service environment. When running a subcommand by hand, pass
the variables it needs (`BOT_HEALTH_CONFIG`, `BOT_NUT_*`, ...) explicitly, use
`sudo -E`, or source the env file inside the elevated shell (this keeps secrets
off the command line). Sourcing runs the file as shell, so any value with
spaces or shell metacharacters (`$`, `&`, `;`, `#`, ...) must be shell-quoted
in the env file; systemd's `EnvironmentFile` reads the same file with its own
quoting rules, so prefer values that need no quoting:

```bash
sudo sh -c 'set -a; . /usr/local/etc/tgshd.conf; exec tgshd check --once'
sudo env BOT_HEALTH_CONFIG=/usr/local/etc/tgshd-health.yaml tgshd heartbeat --planned-reboot 900
```

Without `BOT_HEALTH_CONFIG`, `tgshd heartbeat` writes the default
`/run/tgshd/heartbeat`; pass the variable whenever `heartbeat_file` is
customised.
It also exits 1 when `BOT_HEALTH_CONFIG` points at an unreadable or
unparsable file, since the heartbeat path cannot then be determined.

### Heartbeat and host requirements

The daemon writes `ts=<epoch> planned_until=<epoch>` to
`general.heartbeat_file` every 30 seconds. An external watchdog should treat a
missing file or an old `ts` as "daemon quiet", unless `now < planned_until`.
`tgshd heartbeat` keeps an existing `ts`, and on a missing or corrupt file
writes the current time as `ts`.

Run tgshd as root on the host itself (not in the distroless container) so it
can read host state; the state database and instance lock live in
`general.state_dir` (default `/var/lib/tgshd`).

### Memory protection

A runaway service can starve the whole host: the kernel starts thrashing, even
SSH stalls, and the OOM killer arrives too late or picks the wrong victim.
The `memory` section adds two independent parts. It is optional; without it
there is no memory check and no guard.

- **The check** (every `interval`) warns early, in `mode`: available memory
  below `warn_available_percent`, swap above `warn_swap_percent`, pressure
  above `warn_pressure_percent`, a service that grew by `growth_warn` within
  `growth_window`, and a container running without a memory limit (except
  those in `audit_exclude`). Messages name the biggest consumer, for example
  `PROBLEM: memory: 9% available (5.6 of 62 GB); top: web-app 31 GB`. A
  service that is big but flat (a database) or a cache that swings up and down
  does not trigger the growth warning.
- **The guard** reads the kernel directly, about once a second, and does not
  wait for the check's schedule. When available memory is below
  `danger_available_percent` **and** the full-stall pressure is at or above
  `danger_pressure_percent` for `danger_for`, it picks the service that grew
  most within `culprit_window` (at least `culprit_min_growth`) and restarts it
  (`docker restart`, or `systemctl restart` for a systemd unit). Docker and
  systemctl get exactly `action_timeout` to finish; if the restart has not
  finished by then, the guard kills the service's cgroup through the kernel
  and reports that. If the same service runs away
  again within `repeat_window`, it is **stopped** instead of restarted, and
  you are told by `failed_mode` (SMS by default). If memory does not recover
  within `recovery_window`, it says so once and does not act a second time in
  the same episode. It never touches the services in `never_touch`, nor
  tgshd's own service, nor the core services it needs to keep the host
  reachable and to act at all: `docker.service`, `containerd.service`,
  `dbus.service`, `ssh.service`, `sshd.service` and every `systemd-*` unit
  (these are built in and cannot be overridden). When the culprit is
  protected, or no service is growing, it only reports (with the top
  process).
- **`never_touch` names** are container names (`db`) or systemd unit names,
  either in full (`backup.service`) or, as `systemctl` accepts them, without
  `.service` (`backup` protects `backup.service` too). A bare name protects
  both a container and a unit of that name. Names are matched exactly, with
  no globs.

The guard never pauses in quiet windows (`pause_in_quiet_windows` applies to
the warnings only). Its reports are queued and retried like other alerts and
may arrive after the action.

**Roll out in `report` first.** `guard.intervene` is `off`, `report` (the
default) or `act`. In `report` the guard runs the same logic but only sends
`would restart ...` (or `would stop ...`) messages, by `mode`, so the preview
itself is quiet. Two cases are not previews and go out by `failed_mode` (SMS
by default) in `report` as in `act`: a danger whose culprit is protected, and
a danger with no growing service to blame. That is deliberate: tgshd could
not have fixed either, so only you can. Leave the guard in `report` for a
while, check that its messages name the service you would have chosen
(adjust `never_touch` if not), and only then set `act`.

```yaml
memory:
  guard:
    intervene: act
    never_touch: [ "db", "backup.service" ]
```

`tgshd check --once` runs the memory check like the others and prints its
readings (`memory/available`, `memory/swap`, `memory/pressure`, per-service
growth and the container limits), then a `GUARD memory/guard: <mode>` line
with the configured `intervene` mode (`act` and `report` carry the note
`(configured; check --once never starts it)`). It never starts the guard, acts
or locks memory.

**Host requirements**

- cgroup v2 (the unified hierarchy). Without it the guard does not start and a
  checker error says so. Killing a service's cgroup as a last resort needs
  Linux 5.14 or newer.
- Memory accounting on the services: the `system.slice` service cgroups must
  expose `memory.stat` (systemd's `DefaultMemoryAccounting=yes`, the default
  on cgroup v2). If none does, or there is no service cgroup at all, the
  guard does not start, in `act` too, and a checker error says so.
- Memory pressure information (PSI): `/proc/pressure/memory` must exist. If
  your kernel was started with `psi=0`, enable PSI. Without it the guard does
  not start and the check reports a checker error.
- Docker with the `systemd` cgroup driver (the default on cgroup v2), so
  containers sit under `system.slice` as `docker-<id>.scope`. With the
  `cgroupfs` driver containers live under `/sys/fs/cgroup/docker/` and are
  neither sampled nor guarded.
- tgshd runs as root on the host (see above), so it can read other processes
  and restart containers and units.
- Shield tgshd from starvation: the shipped
  [`config/tgshd.service`](config/tgshd.service) sets `OOMScoreAdjust=-1000`
  and `LimitMEMLOCK=infinity`, and the guard locks its own memory so it keeps
  running while the host thrashes. Add the same two lines to your own unit
  file if you do not use the shipped one. A failure to lock memory is a
  checker error, not fatal.

**Things to know**

- An invalid `memory` section is a checker error (`config/memory/invalid`);
  the daemon still starts, without the memory check and without the guard.
- If tgshd cannot determine its own cgroup (or runs in the root cgroup), it
  cannot be sure it will not restart itself. The guard then falls back to
  `report` even though `act` is configured, and says so in a checker error.
- A new container is unguarded for up to about 30 s, until its name is
  known (names are refreshed every 30 s tick).
- Kernel out-of-memory kills are reported by the `kernel` check, with the
  process, the container or service and the size, e.g. `Kernel: out of
  memory, killed python3 (pid 4242) in container web-app, 54 GB`, including
  kills found in the previous boot's log. A kill in a container that no longer
  exists, and any kill seen under `tgshd check --once` (which cannot look
  containers up), shows the container's short id instead of its name.
- `/status` shows a memory block (available, swap, stall, top consumers and
  the guard mode with its last action).

## Incoming SMS through gammu

With `BOT_GAMMU_DSN` set, tgshd reads the gammu-smsd `inbox` table every 15
seconds, forwards each received SMS to `TELEGRAM_CHAT_ID` (multi-part texts
joined; parts still missing after 10 minutes are forwarded as incomplete) and
marks its rows `Processed`. This runs with or without health monitoring. If
PostgreSQL is unreachable the daemon still starts; each poll logs an error and
the messages are forwarded once the database is back.

**Remove gammu-smsd's `runonreceive` hook.** tgshd now owns incoming SMS. Delete
the `RunOnReceive` line from the gammu-smsd configuration (its `[smsd]`
section) and restart gammu-smsd; otherwise every message is delivered twice,
once by the hook and once by tgshd.

**Modem probe.** With health monitoring on, a `modem_probe` section proves the
modem and SIM still work: once a week (`weekday`, `time`) tgshd sends an SMS
with a random token to the modem's own number, and the inbox poll recognises
the reply (sender compared by digits, by the last 9 digits when the lengths
differ; exact text) and does not forward it. A PROBLEM is raised at once when
the send fails (it is retried every minute until it goes out), when the reply
has not come back within `reply_within`, or when no probe has come back for
`max_silence` (which must be longer than a week plus `reply_within`); the next
returned probe resolves it.
`phone_env` names the environment variable holding the modem's number, so the
number stays out of the YAML:

```yaml
modem_probe:
  phone_env: MODEM_PHONE   # MODEM_PHONE=+10000000000 in the service environment
  weekday: sunday
  time: "11:02"
  reply_within: 30m
  max_silence: 8d
```

The probe needs `BOT_GAMMU_DSN`; without it the section is reported as a
checker error and not run. Its state survives restarts. `tgshd check --once`
never sends the probe: it prints the stored probe state instead.

**Verifying it.**

1. Remove `RunOnReceive` from the gammu-smsd configuration and restart
   gammu-smsd.
2. Send an SMS to the modem: it is forwarded to Telegram once, and its `inbox`
   row has `Processed = true`.
3. Set the probe's `weekday`/`time` a few minutes ahead and restart tgshd: the
   probe is sent and comes back, it is not forwarded, and `/status` stays
   green (a passing probe sends no alert).

## Build & run

```bash
task setup   # download dependencies
task build   # compile bin/tgshd
task test    # run unit tests

cp .env.dist .env   # then fill it in
./bin/tgshd
```

## Docker

The image is a multi-stage, statically-linked (`CGO_ENABLED=0`) build on a
distroless **nonroot** base. `CAP_NET_RAW` is granted to the binary via `setcap`
so privileged ICMP ping works without running as root.

```bash
docker build -t tgshd:latest .
docker run --rm --stop-timeout 50 --env-file .env tgshd:latest
```

Give the container more than the daemon's 40 s stop budget (see
[Stopping](#stopping)): Docker's default grace is 10 s before it sends
`SIGKILL`. Use `--stop-timeout 50` with `docker run`, or
`stop_grace_period: 50s` in a compose file.

`NET_RAW` is in Docker's default capability set, so no extra `--cap-add` is
required for `/ping`.

## Deploying on a server (systemd)

A unit file is provided at [`config/tgshd.service`](config/tgshd.service). It
runs `/usr/local/bin/tgshd` and reads its environment from
`/usr/local/etc/tgshd.conf`.

```bash
sudo install -m 0755 bin/tgshd /usr/local/bin/tgshd
sudo install -m 0600 .env /usr/local/etc/tgshd.conf
sudo install -m 0644 config/tgshd.service /etc/systemd/system/tgshd.service
sudo systemctl daemon-reload
sudo systemctl enable --now tgshd
```

The unit sets `TimeoutStopSec=50`, above the daemon's stop budget (see
[Stopping](#stopping)); keep it above 40 s if you change it.

## Stopping

On `SIGTERM` the daemon stops within 40 s (its fx stop timeout). In that time
it lets a running health tick end (up to 10 s), has the memory guard deliver
its last outcomes (up to 5 s) and write its intervention records (up to 5 s),
and writes the health state one final time, so events and queued messages
since the last tick reach the next start. Whatever supervises it must allow
more than 40 s before killing it: `TimeoutStopSec=50` in the systemd unit,
`--stop-timeout 50` or `stop_grace_period: 50s` for a container.

## Development

```bash
task format         # gofumpt + golangci-lint --fix
task lint           # golangci-lint (the CI gate)
task test:coverage  # tests with the race detector + coverage profile
```

See [CLAUDE.md](.claude/CLAUDE.md) for the architecture overview, the optional-feature
wiring pattern, and contribution conventions.

## License

See [LICENSE](LICENSE).
