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
  woken. `disks` and `kernel` read
  `/dev/*` and `/dev/kmsg`, so run the daemon as root (the systemd unit below).
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
check, by SMS). Alerts are state based: `PROBLEM` when a condition goes bad for
longer than its threshold, `RESOLVED` when it recovers, and the state survives
restarts. It is **off** unless `BOT_HEALTH_CONFIG` points at a health YAML file;
start from [`config/health.example.yaml`](config/health.example.yaml), which
documents every section.

```bash
sudo install -d /usr/local/etc
sudo install -m 0644 config/health.example.yaml /usr/local/etc/tgshd-health.yaml
# then, in /usr/local/etc/tgshd.conf:
#   BOT_HEALTH_CONFIG=/usr/local/etc/tgshd-health.yaml
sudo systemctl restart tgshd
```

The file is strict YAML: an invalid entry is reported as a checker error and
skipped, and a file that cannot be parsed disables health monitoring. Omitted
sections take their defaults and a check type that is absent is not run.
Durations use Go syntax plus `d` for days (`8d`). Each check accepts `interval`,
`threshold`, `mode` (`tlg`, `sms` or `tlg-or-sms`) and `needs_internet`.
The `timeout` of `http` and `command` entries is at most `20s`, so a hung probe
is judged inside the 40 s run budget of a cycle (a PROBLEM, and a strike
towards a `command` fix) rather than abandoned.
Checker errors are also emailed when `BOT_SMTP_ADDR` is set.

**Checks.** Each check type runs when its section is configured: `network`
(connectivity, evaluated first every cycle), `ups` (NUT power, using the
`BOT_NUT_*` settings), `disks` (SMART via smartmontools), `temperatures`
(hwmon sensors, and GPUs via nvidia-smi), `kernel` (EDAC counters and the
kernel log), `containers` (Docker), `http` (endpoint probes), `freshness`
(age of a status file: its mtime or a Unix time it holds), `command` (custom
probe commands with optional fix actions) and `modem_probe` (a weekly SMS round
trip; it needs `BOT_GAMMU_DSN`). A `summary` section adds the weekly summary
described below.

**Telegram commands.** `/status` lists open problems (or `All checks OK`) and
notes a degraded store or quiet window; `/clear <condition key>` drops an open
condition without sending RESOLVED; `/retire <serial>` retires a drive that was
removed (the full serial or its last 4+ characters; an unknown or ambiguous
value lists the known drives) and clears its conditions. `/history [check] [24h|7d|30d]` lists the
alert log newest first (a check name or condition key prefix filters it; the
default period is 7d), `/temp` shows each sensor's current value next to its
7-day min/avg/max, and `/disks` shows each drive by bus, model and the last 4
serial characters with its health (OK, FAILING or missing), wear and temperature (each
followed by its 7-day min/avg/max, e.g. `temp 41°C (7d 35/38/44)`), spare (NVMe),
data written in the last 7 days and the age of its newest
self-test. These three read the state database; while the daemon runs from
memory they reply that history is unavailable. All commands are accepted only
from `TELEGRAM_CHAT_ID` and `TELEGRAM_ALLOWED_IDS`.

**Weekly summary.** With a `summary` section (default Monday 09:00, host local
time) the daemon pushes one message, prefixed with the host name, listing each
drive (health, wear, data written this week), a services block (containers,
http, freshness and command checks: OK or how many problems are open) and the
open problems. It fits one Telegram message and is cut with `…` when longer; a
section that cannot be built reads "unavailable". Once a day the daemon also
prunes the state database: alerts after 365 days, raw samples after 90 days
(rolled up into daily min/avg/max that `/temp` and `/disks` keep using) and
undelivered messages after 7 days (each dropped one is logged).

**CLI subcommands.**

| Invocation | What it does |
| :--- | :--- |
| `tgshd` | Runs the daemon; health monitoring runs when `BOT_HEALTH_CONFIG` is set |
| `tgshd check --once` | Runs every configured check once with the real adapters and prints `<VERDICT> <key>: <text>` per reading plus a summary line; sends and writes nothing. Exit 0 ran, 1 config/setup error |
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

**Heartbeat.** The daemon writes `ts=<epoch> planned_until=<epoch>` to
`general.heartbeat_file` every 30 seconds. An external watchdog should treat a
missing file or an old `ts` as "daemon quiet", unless `now < planned_until`. `tgshd heartbeat` keeps an existing `ts`, and on a
missing or corrupt file writes the current time as `ts`.

**Host requirements.** Run tgshd as root on the host itself (not in the
distroless container) so it can read host state; the state database and
instance lock live in `general.state_dir` (default `/var/lib/tgshd`).

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
the send fails, when the reply has not come back within `reply_within`, or when
no probe has come back for `max_silence`; the next returned probe resolves it.
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
docker run --rm --env-file .env tgshd:latest
```

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
