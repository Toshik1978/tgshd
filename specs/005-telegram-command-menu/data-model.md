# Data Model: Telegram Command Menu

No persisted data. All values are in-memory and rebuilt at every start.

## BotCommand (`pkg/telegram/route`)

| Field | Type | Rule |
| --- | --- | --- |
| `Name` | string | `^[a-z0-9_]{1,32}$`, without `/` |
| `Description` | string | 1–256 characters, one line |

- `Valid() error` reports the first broken rule.
- Returned by `telegram.Handler.Commands()`; a handler serving several names (the script
  handler) returns several.

## MenuScope (`pkg/telegram/route`)

| Field | Type | Meaning |
| --- | --- | --- |
| `Kind` | `ScopePrivate` \| `ScopeGroupMember` | which Telegram scope |
| `ChatID` | int64 | private: the user id; group member: the group id |
| `UserID` | int64 | group member only: the user id |

Built by `(*AccessList).MenuScopes() []MenuScope`:

- every positive id `u` → `{ScopePrivate, u, 0}`;
- every negative id `g` × every positive id `u` → `{ScopeGroupMember, g, u}`;
- sorted (private first, then by chat id, then user id) for deterministic publishing and tests;
- zero ids never appear (already excluded by `NewAccessList`).

## Command set (`pkg/telegram/route`)

`BuildCommands(builtins, script []BotCommand) (routed, menu []BotCommand, problems []string)` —
the **only** place commands are validated, ordered and de-duplicated:

- `builtins` sorted by name, then `script` in its own order;
- drops invalid entries and repeated names (first wins, so built-ins beat the script), one
  problem string each;
- `routed` is every surviving entry — the consumer registers exactly these;
- `menu` is the first 100 of `routed`, with one problem string if capped (FR-012).

The consumer logs each problem as a warning and does no filtering of its own.

## Script command line (`pkg/command`)

`parseScriptCommands(out string) (cmds []route.BotCommand, problems []string)`:

- one entry per non-blank line, `name - description`, split on the first `" - "`;
- name and description trimmed; one leading `/` dropped from the name;
- a line without `" - "` → skipped, problem names the line;
- name and description rules are **not** checked here: `route.BuildCommands` does that for
  built-ins and script entries alike.
