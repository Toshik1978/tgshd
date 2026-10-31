# tgshd — Agent & Developer Guide

`tgshd` (Telegram Shell Daemon) is a small Go daemon for Ubuntu servers. It
processes commands sent over Telegram and runs periodic background checks
(ping, speedtest, UPS/power via NUT, ZTE MC888 modem SMS, and custom scripts).

---

## CLI Command Reference

All automation runs through [go-task](https://taskfile.dev) (`Taskfile.yml`).

```bash
task setup          # download Go module dependencies
task lint           # run golangci-lint (the CI gate)
task format         # gofumpt + golangci-lint --fix
task test           # run all unit tests
task test:coverage  # run tests with the race detector + coverage profile
task build          # compile the tgshd binary (bin/tgshd)
task clean          # remove build artifacts and the test cache
```

---

## Architecture

`tgshd` is wired with [uber/fx](https://github.com/uber-go/fx). `cmd/main.go` is
the composition root; it assembles three kinds of components and runs them under
an fx lifecycle:

1. **Telegram consumer** (`pkg/telegram`) — long-polls Telegram, routes each
   incoming command to a handler, and publishes replies.
2. **Command handlers** (`pkg/command`) — one per command, collected into the fx
   result group `command_handler`. Each implements `telegram.Handler`
   (`Name() string`, `Enabled() bool`, `Handle(ctx, origin route.Origin, cmd string) error`).
3. **Background workers** (`pkg/sms`, …) — collected into the fx result group
   `worker`. Each implements `app.Worker` (`Name`, `Duration`, `Do`) and is run
   on a schedule by `cmd/app` via gocron.

Business logic sits behind small interfaces (`command.Publisher`, `command.Power`,
`command.Pinger`, `command.Speedtest`, `command.Script`, `command.ZTE`,
`command.SmsSender`, `sms.ZTE`, …). The concrete implementations
(`pkg/ping`, `pkg/power`, `pkg/speedtest`, `pkg/zte`, `pkg/script`,
`pkg/sms/gammu`)
are thin adapters over external systems.

### Optional features are env-gated

Features that need external hardware/services are wired **only when configured**,
so the daemon always starts even if they are absent. The pattern lives in
`main()`:

```go
if os.Getenv("BOT_ZTE_HOST") != "" {
    options = append(options, zteProviders(), zteWorkers())
}
if os.Getenv("BOT_GAMMU_DSN") != "" {
    options = append(options, gammuProviders())
}
```

When adding a new optional feature, follow this pattern: a `xxxProviders()`
`fx.Option` that provides the adapter + its command/worker into the right result
group, gated behind an env-var check in `main()`.

### Package map

| Package | Responsibility |
| :--- | :--- |
| `cmd` | `main.go`: fx composition root, config, env-gated wiring; `cli.go`: `check --once` and `heartbeat` subcommands |
| `cmd` (health) | `health.go`, `health_adapters.go`, `health_hardware.go`, `health_services.go`, `health_sms.go`, `health_reports.go`, `health_memory.go`, `health_memory_check.go`: health wiring, adapters and report/summary providers |
| `cmd/app` | `Application` lifecycle, gocron worker scheduler |
| `pkg/telegram` | Telegram consumer (long-poll, dispatch to handlers) + publisher (replies) |
| `pkg/telegram/route` | Pure Telegram destinations and routing: `Route`/`Topic` and `TELEGRAM_TOPIC_*` parsing, command `Origin`, `AccessList` allow-list and group rule, `CommandName` (`/name@bot`), thread-rejection fallback (`ThreadRejected`/`SendWithFallback`) |
| `pkg/command` | Command handlers (`/ping`, `/power`, `/speed`, `/5g` `/4g` `/3g`, `/sms`, `/status`, `/clear`, `/retire`, `/history`, `/temp`, `/disks`, script) |
| `pkg/ping` | ICMP ping (pro-bing) |
| `pkg/power` | UPS voltage via NUT |
| `pkg/speedtest` | Internet speed test |
| `pkg/script` | Runs a user-provided script for unknown commands |
| `pkg/zte` | ZTE MC888 modem HTTP client (network switch, SMS read) |
| `pkg/sms` | Incoming-SMS worker (modem and gammu inbox → Telegram) + GSM encoding |
| `pkg/sms/gammu` | Outgoing and incoming SMS via the gammu-smsd PostgreSQL outbox/inbox |
| `pkg/health` | Pure alert engine: readings, conditions, thresholds, quiet windows, rate limits (no I/O) |
| `pkg/health/config` | Strict YAML loader and validation for `BOT_HEALTH_CONFIG` (`config/health.example.yaml`) |
| `pkg/health/notify` | Alert and checker-error delivery (Telegram, SMS, email) |
| `pkg/health/runner` | 30 s tick: heartbeat, scheduling, bounded runs, fix actions, persistence, drive retire |
| `pkg/health/report` | Telegram reply and weekly-summary text (`/status`, `/history`, `/temp`, `/disks`, summary) |
| `pkg/health/memguard` | Pure memory-guard decision loop: danger detection, culprit choice, restart/stop/kill actions, episode state (no I/O; actuator, reporter and clock are interfaces) |
| `pkg/health/check/memory` | `memory` check: available/swap/pressure, per-service growth, container memory-limit audit |
| `pkg/health/store` | SQLite state and history store that survives restarts |
| `pkg/health/check/*` | One pure package per check type (`network`, `ups`, `disks`, `temps`, `kernel`, `memory`, `containers`, `httpcheck`, `freshness`, `command`, `modemprobe`); I/O behind small source interfaces |
| `pkg/host/*` | Thin host adapters behind the check sources: `smartctl`, `nvidia`, `hwmon`, `edac`, `kmsg`, `sysfs`, `docker`, `execprobe`, `journal` (`journalctl`, previous-boot kernel tail), `procmem` (`/proc` memory, pressure, per-process RSS), `cgroupmem` (cgroup v2 service memory, `cgroup.kill`), `systemctl` (restart/stop units), `httpstamp` (freshness stamp over HTTP), `lockfile` (heartbeat and state lock) |
| `pkg/mail` | Plain-text email through an SMTP relay |
| `pkg/doh` | Minimal RFC 8484 DNS-over-HTTPS resolver (used by the `http` check) |

---

## Conventions

These are non-negotiable for changes in this repo. They are the working summary of the
project constitution, `.specify/memory/constitution.md`, which wins on any conflict.

1. **Idiomatic Go.** Standard library first; wrap errors with `%w`; keep packages
   focused; follow existing patterns in the file you are editing.
2. **Tests use the standard library `testing` package** — table-driven where it
   helps, with hand-written fakes for the interfaces above. Do **not** add
   testify or any other test dependency.
3. **New third-party dependencies require approval.** State the package, what it
   solves, and why the standard library is insufficient before adding it.
4. **Commit style.** Plain, capitalized imperative subjects (e.g. `Add gammu SMS
   builder`). No conventional-commit prefixes. Work is committed directly on
   `main` in this repo.
5. **`CGO_ENABLED=0`.** The Docker image is distroless and static; do not
   introduce a CGO dependency.

---

## Testing coverage

The CI coverage gate (80% floor) measures the packages that can be cleanly
unit-tested. The scope is the `COVERPKG` variable in `Taskfile.yml`, repeated
as `-coverpkg` in `.github/workflows/ci.yml` (keep the two in sync):
`pkg/command/...`, `pkg/sms`, `pkg/script`, `cmd/app`, `pkg/health/...` and `pkg/telegram/route`.
Everything that only runs against real hardware/services is excluded from the
gate: the I/O adapters (`pkg/ping`, `pkg/power`, `pkg/speedtest`, the Telegram
transport), the host adapters (`pkg/host/...`, `pkg/mail`, `pkg/doh`), the ZTE
device HTTP client (`pkg/zte`), the gammu Postgres repository
(`pkg/sms/gammu`), and the fx root (`cmd`). Their tests still run in CI
(`go test ./...`) — e.g. the gammu message-part builder is unit-tested — they
just don't count toward the coverage gate.

---

## Spec Kit workflow

New features are planned with Spec Kit and executed with superpowers:
`/speckit-specify` → `/speckit-clarify` → `/speckit-plan` → `/speckit-tasks` →
`/speckit-analyze`, then `/speckit-superpowers-execute` (imports `tasks.md` into beads and runs
it test-first). Do not use `/speckit-implement`. Feature artifacts live in `specs/NNN-name/`;
amend the constitution only with `/speckit-constitution`.
