# Contract: environment

| Variable | Default | Meaning |
| --- | --- | --- |
| `BOT_TELEGRAM_POLL` | `true` | `false`: send-only — no `getUpdates`, no command menu, no commands; alerts, summaries, topics, SMS and email still go out |

Accepted values: anything `strconv.ParseBool` accepts (`1`, `t`, `true`, `0`, `f`, `false`, …,
case variants). Another value fails the start with the environment parse error, like any other
malformed `BOT_*` value.
