# Contract: Telegram Command Menu

## Handler interface (`pkg/telegram`)

```go
type Handler interface {
    // Commands returns the commands this handler serves, with menu descriptions.
    Commands() []route.BotCommand
    Enabled() bool
    Handle(ctx context.Context, origin route.Origin, cmd string) error
}
```

`Name() string` and its `|`-separated names are removed.

## Built-in menu entries

| Command | Description (≤ 256 chars) |
| --- | --- |
| `ping` | Ping the configured hosts |
| `power` | Show UPS input voltage |
| `speed` | Run an internet speed test |
| `5g` / `4g` / `3g` | Switch the modem to 5G / 4G / 3G |
| `sms` | Send SMS: /sms <phone> <text> |
| `status` | Show current health status |
| `clear` | Clear a condition: /clear <condition key> |
| `retire` | Retire a drive: /retire <serial or last 4+ chars> |
| `history` | Alert history: /history [check] [24h\|7d\|30d] |
| `temp` | Show temperatures |
| `disks` | Show disk health |

Wording is final in implementation; every entry MUST pass `BotCommand.Valid`, checked by a test.

## Calls at start (after the first successful `getMe`)

1. `deleteMyCommands` with `scope = {type: default}`.
2. For each `MenuScope` from the allow-list, `setMyCommands` with the menu and:
   - private: `scope = {type: chat, chat_id: <user id>}`
   - group member: `scope = {type: chat_member, chat_id: <group id>, user_id: <user id>}`

No other scope is ever written. Each failed call logs a warning with scope type, chat id, user
id and the error, and the next call proceeds. The menu is skipped entirely (only step 1 runs)
when there are no scopes. Command handling starts without waiting for these calls.

## Clearing a stale menu by hand (README)

```sh
curl -s "https://api.telegram.org/bot$TELEGRAM_TOKEN/deleteMyCommands" \
  -H 'Content-Type: application/json' \
  -d '{"scope":{"type":"chat_member","chat_id":-1000000000000,"user_id":100000000}}'
```

Use `{"type":"chat","chat_id":<user id>}` for a private chat.
