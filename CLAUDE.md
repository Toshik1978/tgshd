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
   (`Name() string`, `Enabled() bool`, `Handle(ctx, senderID, cmd) error`).
3. **Background workers** (`pkg/sms`, …) — collected into the fx result group
   `worker`. Each implements `app.Worker` (`Name`, `Duration`, `Do`) and is run
   on a schedule by `cmd/app` via gocron.

Business logic sits behind small interfaces (`command.Publisher`, `command.Power`,
`command.Pinger`, `command.Speedtest`, `command.Script`, `command.ZTE`,
`command.SmsSender`, `sms.ZTE`, …). The concrete implementations
(`pkg/ping`, `pkg/power`, `pkg/speedtest`, `pkg/zte`, `pkg/script`, `pkg/sms/gammu`)
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
| `cmd/main.go` | fx composition root, config, env-gated wiring |
| `cmd/app` | `Application` lifecycle, gocron worker scheduler |
| `pkg/telegram` | Telegram consumer (routing) + publisher (replies) |
| `pkg/command` | Command handlers (`/ping`, `/power`, `/speed`, `/5g` `/4g` `/3g`, `/sms`, script) |
| `pkg/ping` | ICMP ping (pro-bing) |
| `pkg/power` | UPS voltage via NUT |
| `pkg/speedtest` | Internet speed test |
| `pkg/script` | Runs a user-provided script for unknown commands |
| `pkg/zte` | ZTE MC888 modem HTTP client (network switch, SMS read) |
| `pkg/sms` | Incoming-SMS worker (modem → Telegram) + GSM encoding |
| `pkg/sms/gammu` | Outgoing SMS via a gammu-smsd PostgreSQL outbox |

---

## Conventions

These are non-negotiable for changes in this repo.

1. **Idiomatic Go.** Standard library first; wrap errors with `%w`; keep packages
   focused; follow existing patterns in the file you are editing.
2. **Tests use the standard library `testing` package** — table-driven where it
   helps, with hand-written fakes for the interfaces above. Do **not** add
   testify or any other test dependency.
3. **New third-party dependencies require approval.** State the package, what it
   solves, and why the standard library is insufficient before adding it.
4. **Commit style.** Plain, capitalized imperative subjects (e.g. `Add gammu SMS
   builder`). No conventional-commit prefixes and **no `Co-Authored-By` or other
   AI-attribution trailers**. Work is committed directly on `main` in this repo.
5. **`CGO_ENABLED=0`.** The Docker image is distroless and static; do not
   introduce a CGO dependency.

---

## Testing coverage

The CI coverage gate measures the logic packages that can be unit-tested
(`task test:coverage`'s `-coverpkg` scope): `pkg/command`, `pkg/sms`,
`pkg/sms/gammu`, `pkg/zte`, `pkg/script`, and `cmd/app`. The thin I/O adapters
(`pkg/ping`, `pkg/power`, `pkg/speedtest`, the Telegram transport) and the fx
root (`cmd/main.go`) run only against real hardware/services and are exercised in
deployment, not the unit gate.
