---

description: "Task list for Telegram command menu"
---

# Tasks: Telegram Command Menu

**Input**: Design documents from `/specs/005-telegram-command-menu/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md

**Tests**: Every task is implemented test-first (Constitution II): each task writes its own tests,
standard library `testing` only, table-driven with hand-written fakes. There are no standalone
test tasks.

**Public repository**: tests, docs and examples use placeholder ids only (users `100000000`,
`200000000`; group `-1000000000000`); no real chat, group or host names.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel (different files, no dependency on an incomplete task)
- **[Story]**: US1, US2, US3 from spec.md

---

## Phase 1: Foundational (Blocking Prerequisites)

**Purpose**: pure command, scope and script-format logic, then the switch of every handler from
`Name()` to `Commands()` in two small steps. Each task leaves the build and all tests green; no
menu is published yet.

- [x] T001 [P] Add `BotCommand{Name, Description string}` with `Valid() error` ("name `^[a-z0-9_]{1,32}$`, without `/`"; "description 1–256 characters, one line") and `BuildCommands(builtins, script []BotCommand) (routed, menu []BotCommand, problems []string)` in pkg/telegram/route/command.go per data-model.md — the only place commands are validated, ordered and de-duplicated: "`builtins` sorted by name, then `script` in its own order"; "drops invalid entries and repeated names (first wins, so built-ins beat the script), one problem string each"; "`routed` is every surviving entry"; "`menu` is the first 100 of `routed`, with one problem string if capped"; table tests in pkg/telegram/route/command_test.go (FR-001, FR-009, FR-011, FR-012, R3, R8)
- [x] T002 [P] Add `MenuScope{Kind; ChatID, UserID int64}` with kinds `ScopePrivate` and `ScopeGroupMember`, and `(*AccessList).MenuScopes() []MenuScope` ("every positive id `u` → `{ScopePrivate, u, 0}`"; "every negative id `g` × every positive id `u` → `{ScopeGroupMember, g, u}`"; "sorted (private first, then by chat id, then user id)") in pkg/telegram/route/access.go; table tests in pkg/telegram/route/access_test.go cover users only, groups × users, groups without users (no scopes) and an empty list (FR-005, FR-006, R4)
- [x] T003 [P] Add `parseScriptCommands(out string) (cmds []route.BotCommand, problems []string)` in pkg/command/script_commands.go per data-model.md and contracts/script.md: "one entry per non-blank line, `name - description`, split on the first `\" - \"`"; "name and description trimmed; one leading `/` dropped from the name"; "a line without `\" - \"` → skipped, problem names the line"; blank lines ignored silently; name and description rules are not checked here (left to `route.BuildCommands`); table tests in pkg/command/script_commands_test.go include the old `a|b|c` line being skipped (FR-008, FR-010, R7)
- [x] T004 Add `Commands() []route.BotCommand` beside the existing `Name()` on every handler, without changing the `telegram.Handler` interface yet: export one `route.BotCommand` per built-in from pkg/command (e.g. in pkg/command/entity.go) with the descriptions in contracts/telegram.md, and return it from each handler in pkg/command/*.go; the cmd fallbacks `replyCommand` (cmd/health_reports.go) and `noDrivesCommand` (cmd/health_hardware.go) return the same exported entries; rename `Script.Name` to `Script.List` in pkg/command/command_unknown.go and pkg/script/script.go; `unknownCommand` parses the script output with `parseScriptCommands` at construction, logs each problem as a warning, returns the parsed entries from `Commands()`, keeps `Name()` working for now by joining the parsed names with `|`, and implements a small optional marker interface identifying it as the script handler; add tests in pkg/command/*_test.go, pkg/script/script_test.go and cmd/*_test.go that each handler's `Commands()` returns its entries and that every exported built-in entry passes `BotCommand.Valid` (FR-001, FR-003, FR-008, R6, R7)
- [x] T005 Switch `telegram.Handler` to `Commands() []route.BotCommand`, `Enabled() bool`, `Handle(…)` in pkg/telegram/consumer.go per contracts/telegram.md and delete every `Name()` method from pkg/command/*.go, cmd/health_reports.go and cmd/health_hardware.go: `consumer.Start` splits enabled handlers into built-ins and the script handler (by the marker interface from T004), calls `route.BuildCommands`, logs each problem as a warning, registers one telebot handler per `routed` entry (no filtering of its own; the `|` splitting is removed) and keeps `menu` on the consumer for publishing; `handleCommand` logs the matched command name; update fakes and tests in pkg/telegram/consumer_test.go, pkg/command/*_test.go and cmd/*_test.go, including a consumer test that a script name equal to a built-in routes to the built-in (FR-002, FR-011, FR-013, R6, R8)

**Checkpoint**: builds, all tests pass, commands are routed exactly as before by name; script commands come from `name - description` lines; `Name()` no longer exists.

---

## Phase 2: User Story 1 & 2 — Only allowed users see the real commands (Priority: P1) 🎯 MVP

**Goal**: at start, the default menu is removed and the enabled commands are published to each
allowed user's private chat and to each allowed user inside each allowed group.

**Independent Test**: with a fake command setter, the default scope is deleted first, then one
set call per `MenuScope` with the menu; a failing call is logged and the rest still run; no call
follows a cancelled context; against a real bot, quickstart.md manual steps 2–5.

- [x] T006 [US2] Add a `commandSetter` interface (`SetCommands(opts ...interface{}) error`, `DeleteCommands(opts ...interface{}) error`, satisfied by `*telebot.Bot`) and `publishMenu(ctx context.Context)` in pkg/telegram/menu.go: if `ctx` is not cancelled, call `DeleteCommands(telebot.CommandScope{Type: telebot.CommandScopeDefault})`; a failure is logged as a warning and publishing continues; tests in pkg/telegram/menu_test.go with a fake setter (FR-004, FR-007, R2, R5)
- [x] T007 [US1] Extend `publishMenu` in pkg/telegram/menu.go: for each `AccessList.MenuScopes()` entry, check `ctx` and return if it is cancelled, then call `SetCommands([]telebot.Command, scope)` with the consumer's `menu` and `telebot.CommandScope{Type: CommandScopeChat, ChatID: user}` or `{Type: CommandScopeChatMember, ChatID: group, UserID: user}`; each failure logs a warning with scope type, chat id, user id and error and the next scope proceeds; no set calls when there are no scopes or the menu is empty; log one info line with the number of scopes published; extend pkg/telegram/menu_test.go, including cancellation part-way stopping further calls (FR-005, FR-006, FR-007, R4, R5)
- [x] T008 [US1] In pkg/telegram/consumer.go `connectAndPoll`, after `fetchMe` succeeds start `publishMenu(ctx)` with the run context in its own goroutine, using a `commandSetter` field that defaults to `c.bot` and tests replace, before `c.bot.Start()`, so polling does not wait; publishing runs once per process; `Stop` cancels the run context as today and does not wait for the publish goroutine (a telebot call cannot be interrupted, and a hung call must not hold shutdown); tests in pkg/telegram/consumer_test.go assert one publish after connect, none when the context is cancelled before connect, and that `Stop` returns while a fake publish call is blocked (FR-007, R5, Principle V)

**Checkpoint**: any default menu disappears on start; allowed users see the menu in private and group chats; others see none; shutdown never waits on Telegram.

---

## Phase 3: User Story 3 — Script commands appear with their own descriptions (Priority: P2)

**Goal**: the operator knows the new script output format and can migrate.

**Independent Test**: a script printing two `name - description` lines shows both in the menu
with those descriptions (quickstart.md manual steps 1–3); parsing and routing are covered by
T003–T005.

- [x] T009 [US3] Document the script command list in README.md per contracts/script.md: the `name - description` format with the example script, the rules (name characters and length, description length, blank lines, skipped lines and clashes logged as warnings), that the old `a|b|c` output is no longer accepted, and that commands with parameters should reply with usage when sent bare; update the `TELEGRAM_UNKNOWN_SCRIPT` row in the configuration table (FR-008, FR-010, FR-014)

---

## Phase 4: Polish & Cross-Cutting Concerns

- [ ] T010 Add a "Command menu" section to README.md: tgshd replaces any default (BotFather) menu at every start, who sees it (allowed users in private chats and inside allowed groups; others none), that Telegram cannot limit it to one topic, that commands sent from the menu arrive without parameters, and the `curl` from contracts/telegram.md (placeholder ids) to clear the menu of a user removed from the allow-list (FR-014, R9)
- [ ] T011 [P] Update .claude/CLAUDE.md: the `telegram.Handler` interface is now `Commands() []route.BotCommand`, `Enabled() bool`, `Handle(…)`; the `pkg/telegram/route` package-map entry mentions `BotCommand`, `BuildCommands` and menu scopes; the `pkg/telegram` entry mentions publishing the command menu
- [ ] T012 Run the automated part of specs/005-telegram-command-menu/quickstart.md: `task lint` reports 0 issues, `task test:coverage` passes with the 80% floor, `task build` succeeds with `CGO_ENABLED=0`; scan the diff for real ids, hostnames or names (Principle VI)

---

## Dependencies & Execution Order

- **Phase 1**: T001, T002, T003 in parallel; T004 after T001 and T003; T005 after T004.
- **Phase 2**: after T005 and T002; T006 → T007 → T008 (shared files).
- **Phase 3**: T009 after T005 (documents the shipped format).
- **Phase 4**: T010 after T009 (same file); T011 in parallel with T010; T012 last.

## Parallel Examples

```text
Phase 1:  T001 (route/command.go) | T002 (route/access.go) | T003 (command/script_commands.go)
Phase 4:  T010 (README.md)        | T011 (.claude/CLAUDE.md)
```

## Implementation Strategy

- **MVP**: Phases 1–2 — the reported problem is solved once the menu goes to allowed users only.
- Phase 3 documents the script migration the operator performs before restarting.
- Phase 4 finishes docs and runs the gates.
