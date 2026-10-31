# Implementation Plan: Telegram Command Menu

**Branch**: `005-telegram-command-menu` (work lands on `main`) | **Date**: 2026-10-31 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `/specs/005-telegram-command-menu/spec.md`

## Summary

At every start, once Telegram first answers, tgshd deletes the bot's default (BotFather) command
menu and publishes the enabled commands, with descriptions, only to allowed users: in each user's
private chat and for each user inside each allowed group. The custom script lists its commands in
BotFather's `name - description` format, which replaces the old `a|b|c` output.

**Approach (chosen in brainstorming).** **A — each handler lists its own commands.**
`telegram.Handler.Name() string` becomes `Commands() []route.BotCommand` (name + description).
The consumer uses that one list for routing and for the menu, so `|` splitting goes away and the
two cannot disagree. Pure logic — command validation, menu building (dedupe, cap at 100), scopes
from the allow-list, script-output parsing — lives in `pkg/telegram/route` and `pkg/command`,
inside the coverage gate; the Telegram calls stay a thin adapter in `pkg/telegram`. Trade-off:
every handler and its test change, mechanically; a missed one is a compile error. Rejected:
**B** — keep `Name()` and add an optional `Description()`: multi-name handlers and the script need
a side map from name to description, two sources of truth. Details: [research.md](research.md).

## Technical Context

**Language/Version**: Go (`go.mod` toolchain)

**Primary Dependencies**: existing only — uber/fx, zap, telebot v4 (`Bot.SetCommands`,
`Bot.DeleteCommands`, `CommandScope`, `CommandScopeDefault/Chat/ChatMember`). **No new
dependency.**

**Storage**: none (the menu is Telegram-side state, rebuilt every start).

**Testing**: standard library `testing`, table-driven, hand-written fakes (a fake command setter
for the publisher).

**Target Platform**: Linux server, static `CGO_ENABLED=0` binary, distroless image.

**Project Type**: single Go daemon.

**Performance Goals**: 1 + N Telegram calls per start (N = users + groups × users), off the
polling path.

**Constraints**: Telegram limits — name `[a-z0-9_]{1,32}`, description 1–256 chars, ≤ 100
commands; public repository — placeholder ids only in docs and tests.

**Scale/Scope**: ~14 handlers (12 in `pkg/command`, 2 fallbacks in `cmd`) change signature, in
two steps so each commit stays small and green: add `Commands()` beside `Name()`, then switch the
interface and delete `Name()`; one script contract changes.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | Status |
|---|---|
| I. Idiomatic Go, stdlib first | Pass — small value types, errors wrapped with `%w`, logic in focused packages. |
| II. Test-first, stdlib testing, pure logic in gate | Pass — validation, menu, scopes in `pkg/telegram/route`; script parsing in `pkg/command`; both already in `COVERPKG`. Publisher in `pkg/telegram` tested against a fake. |
| III. Dependency discipline | Pass — none added. |
| IV. CGO-free | Pass. |
| V. Env-gated, daemon always starts | Pass — publishing runs after connect in its own goroutine; every failure is logged, never fatal. |
| VI. Public repository hygiene | Pass — README and tests use `100000000`, `-1000000000000`. |
| VII. Commit discipline | Pass — plain imperative subjects, on `main`. |

Post-design re-check: unchanged, all pass.

## Design

### `pkg/telegram/route` (pure, in the gate)

- `type BotCommand struct{ Name, Description string }` with `Valid() error`.
- `BuildCommands(builtins, script []BotCommand) (routed, menu []BotCommand, problems []string)`
  — the single place commands are validated, ordered (built-ins by name, then script order) and
  de-duplicated (first wins); `menu` is `routed` capped at 100; problems returned for logging.
- `(*AccessList).MenuScopes() []MenuScope` — private scope per positive id; group-member scope
  per negative id × positive id; deterministic order.

### `pkg/command` (in the gate)

- Every handler: `Name()` → `Commands()` returning its entry; descriptions per
  [contracts/telegram.md](contracts/telegram.md); usage in descriptions of commands with
  parameters.
- `unknownCommand`: at construction parses the script's stdout with
  `parseScriptCommands`; logs each problem; `Commands()` returns the parsed list.
- `Script` interface: `Name(ctx)` renamed `List(ctx)` (returns raw stdout).

### `pkg/script`

- `Name` → `List`; behaviour unchanged.

### `pkg/telegram` (adapter)

- `Handler` interface per [contracts/telegram.md](contracts/telegram.md).
- `consumer.Start`: splits enabled handlers into built-ins and the script handler (marked by a
  small optional interface), calls `route.BuildCommands`, logs its problems, registers one
  telebot handler per `routed` entry and keeps `menu` for publishing. No filtering of its own.
- Menu publisher: small `commandSetter` interface (`SetCommands`, `DeleteCommands` — satisfied by
  `*telebot.Bot`); `publishMenu` deletes the default scope, then sets the menu per
  `MenuScope`, logging each failure with scope details.
- `connectAndPoll`: after `fetchMe` succeeds, starts `publishMenu(runCtx)` in a goroutine, then
  starts polling. `publishMenu` checks `runCtx` before each call and returns once it is
  cancelled; `Stop` does not wait for it (a telebot call cannot be interrupted, and a hung call
  must not hold shutdown).
- `handleCommand` logs the matched command name instead of `handler.Name()`.

### `cmd`

- Fallback handlers (`replyCommand`, `noDrivesCommand`) reuse the exported built-in entries from
  `pkg/command`; no wiring or env-var change.

### Docs

- README: new script output format with example, menu behaviour (who sees what, no per-topic
  menus), the `curl` to clear a removed user's menu.
- `.claude/CLAUDE.md`: handler interface now `Commands() []route.BotCommand`; `route` package
  map entry mentions `BotCommand`, menu and scopes.

## Project Structure

### Documentation (this feature)

```text
specs/005-telegram-command-menu/
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── contracts/
│   ├── script.md
│   └── telegram.md
└── tasks.md          # /speckit-tasks
```

### Source Code (repository root)

```text
pkg/telegram/route/
├── command.go        # + BotCommand, Valid, BuildCommands
├── command_test.go
├── access.go         # + MenuScope, MenuScopes
└── access_test.go
pkg/telegram/
├── consumer.go       # Handler.Commands, registration, publish after connect
├── menu.go           # commandSetter, publishMenu
├── menu_test.go
└── consumer_test.go
pkg/command/
├── command_*.go      # Name → Commands
├── script_commands.go       # parseScriptCommands
├── script_commands_test.go
└── command_*_test.go
pkg/script/script.go  # Name → List
cmd/health_reports.go, cmd/health_hardware.go  # fallbacks: Name → Commands
README.md
.claude/CLAUDE.md
```

**Structure Decision**: existing single-project layout; one new file pair in `pkg/telegram`
(menu) and one in `pkg/command` (script parsing).

## Complexity Tracking

No violations.
