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
| *custom* | Names/executes commands from your own script | `TELEGRAM_UNKNOWN_SCRIPT` is set |

**Background workers**

- **Incoming SMS forwarding** — polls the ZTE MC888 modem for new SMS and
  forwards them to your Telegram chat (enabled when the ZTE modem is configured).

- **Host health monitoring** — scheduled checks with PROBLEM/RESOLVED alerts that
  survive restarts (see [Host health monitoring](#host-health-monitoring)).

Every hardware/service-backed feature is **optional**: if it isn't configured,
that feature is simply inactive and the daemon still runs.

## Requirements

- **Go 1.27+** (the binary compiles with `CGO_ENABLED=0`)
- **[go-task](https://taskfile.dev)** — all automation is driven through `Taskfile.yml`
- Optional, per feature: a [NUT](https://networkupstools.org/) server (power),
  a ZTE MC888 modem (network switching + incoming SMS), and a
  [gammu-smsd](https://wammu.eu/smsd/) instance with a **PostgreSQL** backend
  (outgoing SMS).

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
| `BOT_GAMMU_DSN` | PostgreSQL DSN for the gammu-smsd outbox — enables `/sms` |
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
Checker errors are also emailed when `BOT_SMTP_ADDR` is set.

**Checks.** The health file can configure these check types: `network`, `ups`,
`disks`, `temperatures`, `kernel`, `containers`, `http`, `freshness`, `command`
and `modem_probe`, plus a weekly `summary`. Currently the daemon runs `network`
(connectivity, evaluated first every cycle) and `ups` (NUT power, using the
`BOT_NUT_*` settings). The other sections are accepted and validated but do not
run yet; the hardware checks will bring their own host requirements (such as
smartmontools or nvidia-smi) when they land.

**Telegram commands.** `/status` lists open problems (or `All checks OK`) and
notes a degraded store or quiet window; `/clear <condition key>` drops an open
condition without sending RESOLVED. Both are accepted only from
`TELEGRAM_CHAT_ID` and `TELEGRAM_ALLOWED_IDS`.

**CLI subcommands.**

| Invocation | What it does |
| :--- | :--- |
| `tgshd` | Runs the daemon; health monitoring runs when `BOT_HEALTH_CONFIG` is set |
| `tgshd check --once` | Runs every configured check once with the real adapters and prints `<VERDICT> <key>: <text>` per reading plus a summary line; sends and writes nothing. Exit 0 ran, 1 config/setup error |
| `tgshd heartbeat --planned-reboot <seconds>` | Sets `planned_until` in the heartbeat file so an external watchdog tolerates a planned reboot; `0` clears it. Runs no checks. Exit 0 ok, 1 bad argument or write error |

`sudo` drops the service environment. When running a subcommand by hand, pass
the variables it needs (`BOT_HEALTH_CONFIG`, `BOT_NUT_*`, ...) explicitly, use
`sudo -E`, or source the env file inside the elevated shell (this keeps secrets
off the command line):

```bash
sudo sh -c 'set -a; . /usr/local/etc/tgshd.conf; exec tgshd check --once'
sudo env BOT_HEALTH_CONFIG=/usr/local/etc/tgshd-health.yaml tgshd heartbeat --planned-reboot 900
```

Without `BOT_HEALTH_CONFIG`, `tgshd heartbeat` writes the default
`/run/tgshd/heartbeat`; pass the variable whenever `heartbeat_file` is
customised.

**Heartbeat.** The daemon writes `ts=<epoch> planned_until=<epoch>` to
`general.heartbeat_file` every 30 seconds. An external watchdog should treat a
missing file or an old `ts` as "daemon quiet", unless `now < planned_until`.

**Host requirements.** Run tgshd as root on the host itself (not in the
distroless container) so it can read host state; the state database and
instance lock live in `general.state_dir` (default `/var/lib/tgshd`).

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
