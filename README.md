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
| *custom* | Names/executes commands from your own script | `TELEGRAM_UNKNOWN_SCRIPT` is set |

**Background workers**

- **Incoming SMS forwarding** — polls the ZTE MC888 modem for new SMS and
  forwards them to your Telegram chat (enabled when the ZTE modem is configured).

Every hardware/service-backed feature is **optional**: if it isn't configured,
that feature is simply inactive and the daemon still runs.

## Requirements

- **Go 1.26+** (the binary compiles with `CGO_ENABLED=0`)
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
| `TELEGRAM_UNKNOWN_SCRIPT` | Path to a script that lists/executes custom commands |
| `BOT_PING_HOSTS` | Comma-separated hosts for `/ping` |
| `BOT_NUT_IP` | NUT server address |
| `BOT_NUT_NAME` | UPS name in NUT |
| `BOT_NUT_USER` / `BOT_NUT_PASS` | NUT credentials |
| `BOT_NUT_WARN` / `BOT_NUT_ERR` | Voltage warning/error thresholds |
| `BOT_ZTE_HOST` | ZTE MC888 modem address — enables the modem features |
| `BOT_ZTE_PASS` | ZTE modem admin password |
| `BOT_GAMMU_DSN` | PostgreSQL DSN for the gammu-smsd outbox — enables `/sms` |

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

See [CLAUDE.md](CLAUDE.md) for the architecture overview, the optional-feature
wiring pattern, and contribution conventions.

## License

See [LICENSE](LICENSE).
