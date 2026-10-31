# Research: Telegram Command Menu

## R1. Can a menu be scoped to one forum topic?

- **Decision**: No; use the narrowest scopes Telegram offers: `chat` (a user's private chat) and
  `chat_member` (one user inside one group).
- **Rationale**: Bot API `BotCommandScope` has seven types (`default`, `all_private_chats`,
  `all_group_chats`, `all_chat_administrators`, `chat`, `chat_administrators`, `chat_member`);
  none takes a thread id. `chat_member` matches the existing group rule (only allowed senders
  may command the bot in a group).
- **Alternatives considered**: `chat` scope for the whole group (every member sees the menu —
  the reported problem); `chat_administrators` (ties visibility to admin rights, not the
  allow-list).

## R2. Telegram API surface in telebot v4

- **Decision**: `(*telebot.Bot).DeleteCommands(telebot.CommandScope{Type: CommandScopeDefault})`
  once, then `(*telebot.Bot).SetCommands([]telebot.Command{…}, telebot.CommandScope{…})` per
  destination. Both already exist in `gopkg.in/telebot.v4 v4.0.0-beta.10` (`commands.go`).
- **Rationale**: no new dependency (Principle III); both are thin wrappers over `Raw`.
- **Alternatives considered**: calling `Raw("setMyCommands", …)` directly — same result, more
  JSON plumbing.

## R3. Telegram limits on commands

- **Decision**: name `^[a-z0-9_]{1,32}$`; description 1–256 characters; at most 100 commands
  per list. Enforced once, in the pure `route.BuildCommands`; invalid entries are dropped with a
  warning, the menu is capped at 100 with a warning.
- **Rationale**: Telegram rejects the whole `setMyCommands` call if any entry is invalid, so one
  bad script line would otherwise hide the entire menu.
- **Alternatives considered**: sending as-is and logging Telegram's error (one bad line loses
  every menu).

## R4. Users vs. groups on the allow-list

- **Decision**: positive id → user; negative id → group or channel. Destinations: `chat` scope
  for every user id; `chat_member` for every (negative id × user id) pair.
- **Rationale**: Telegram user ids are positive, group/supergroup/channel ids negative; no new
  setting needed. A channel accepts no `chat_member` scope; that call fails and is logged per
  destination (FR-007).
- **Alternatives considered**: a new `TELEGRAM_MENU_*` setting naming groups — extra config for
  information the ids already carry.

## R5. When and how publishing runs

- **Decision**: once per process, in its own goroutine started right after the first successful
  `getMe` in `connectAndPoll`, so polling starts at once. Order: delete default scope, then each
  destination; each failure is logged with scope type, chat id and user id and does not stop the
  rest. The goroutine checks the consumer's run context before every call and returns once it
  is cancelled; `Stop` cancels that context and does **not** wait for the goroutine, because
  telebot's calls cannot be interrupted mid-request and a hung call must not hold shutdown.
  Removing the default scope is kept although the operator already cleared it by hand: it is a
  no-op then, and it undoes any later BotFather edit.
- **Rationale**: FR-007 (no delay to startup or command handling) and Principle V (Telegram
  unreachable must not stop the daemon). The menu is server-side state on Telegram, so once per
  start is enough.
- **Alternatives considered**: synchronously before `bot.Start()` (delays command handling by
  N round trips); on every reconnect (the menu does not change while running).

## R6. Handler contract: one list for routing and menu

- **Decision**: replace `Handler.Name() string` with `Handler.Commands() []route.BotCommand`.
  The consumer registers one telebot handler per entry and publishes the same entries.
- **Rationale**: chosen in brainstorming (approach A). Removes `|` splitting; routing and menu
  cannot disagree (FR-002). `route.BotCommand` lives in the pure `route` package, which both
  `pkg/command` and `pkg/telegram` already import.
- **Alternatives considered**: **B** — keep `Name()` and add optional `Description()`; needs a
  side map for multi-name handlers and the script, two sources of truth.

## R7. Script command list format

- **Decision**: the script, run with no arguments, prints lines `name - description`
  (BotFather's format). Parsing is a pure function in `pkg/command` (in the coverage gate):
  split on the first `" - "`, trim both sides, drop one leading `/` from the name, skip blank
  lines silently, skip lines without ` - ` with a warning naming the line; name and
  description rules are left to `route.BuildCommands`. `pkg/script` keeps returning
  raw stdout; its `Script.Name` method is renamed `Script.List` to say what it returns.
- **Rationale**: the operator already knows the format from BotFather; parsing in the gate keeps
  it table-tested (Principle II).
- **Alternatives considered**: JSON output (harder to write in shell); keeping the `a|b|c`
  fallback (dropped by the operator's decision, FR-010).

## R8. Clashes and ordering

- **Decision**: built-in commands sorted by name first (fx group order is not guaranteed), then
  script entries in script order; the script handler is recognised by a small optional
  interface. A name already taken (built-in or earlier script line) is skipped with a warning.
  All of this happens in `route.BuildCommands`, the single place commands are filtered; the menu
  order is the registration order (stable per configuration).
- **Rationale**: FR-011; a deterministic order makes the menu predictable.
- **Alternatives considered**: alphabetic sort — loses the script author's grouping.

## R9. Stale menus for removed users

- **Decision**: not cleared automatically (clarification 2026-10-31). README documents a one-off
  `curl` to `deleteMyCommands` with a `chat` or `chat_member` scope.
- **Rationale**: removed users cannot run commands anyway; storing destinations adds state for a
  rare case.
