# Quickstart: Telegram Command Menu

## Automated

```bash
task lint            # 0 issues
task test:coverage   # race on, 80% floor over COVERPKG
task build           # CGO_ENABLED=0
```

Key tests:

- `pkg/telegram/route`: `BotCommand.Valid`, `BuildCommands` (order, invalid, duplicate,
  built-in beats script, menu capped at 100 while `routed` keeps all),
  `AccessList.MenuScopes` (users only, groups × users, groups without users, empty list).
- `pkg/command`: `parseScriptCommands` table (valid, leading `/`, blank, no separator, bad name,
  long description, old `a|b|c`); every built-in handler's `Commands()` passes `Valid`.
- `pkg/telegram`: the menu publisher against a fake command setter — default scope deleted
  first, one set call per scope, a failing scope logged and the rest still called, no further
  calls once the run context is cancelled.

## Manual (real bot, placeholder ids)

1. Set `TELEGRAM_ALLOWED_IDS=100000000,-1000000000000` (your user id and the group id) and a
   script printing `wake - Wake a PC: /wake <host>`.
2. Start tgshd; the log shows the menu published to 2 scopes.
3. Private chat with the bot: `/` lists the enabled built-ins plus `wake` with descriptions;
   the Menu button opens the same list.
4. In any topic of the group, as the allowed user: same list. As another member: no commands.
5. The three old BotFather commands are gone from both chats.
6. Start with networking off, then enable it: the daemon starts at once; the menu appears once
   Telegram answers.
