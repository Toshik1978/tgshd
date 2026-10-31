# Contract: Configuration and Custom Script Environment

## Environment variables (read by tgshd)

| Variable | Status | Meaning |
|---|---|---|
| `TELEGRAM_CHAT_ID` | existing, widened | the chat tgshd posts to: a forum supergroup, a channel or a private chat |
| `TELEGRAM_ALLOWED_IDS` | existing, widened | extra allowed chat ids; in a group also the user ids allowed to run commands |
| `TELEGRAM_TOPIC_ALERTS` | new, optional | thread id of the alerts topic |
| `TELEGRAM_TOPIC_SMS` | new, optional | thread id of the SMS topic |
| `TELEGRAM_TOPIC_SYSTEM` | new, optional | thread id of the system topic |

Rules: see [data-model.md](../data-model.md#topics). None set → no topics, no warning. Any set →
each invalid or missing one falls back to no topic with one startup warning naming it.

## Custom script (`TELEGRAM_UNKNOWN_SCRIPT`)

| | Before | After |
|---|---|---|
| argv | one argument, the whole message | unchanged |
| environment | inherited | inherited, plus `TELEGRAM_THREAD_ID=<thread id>` when the command came from a topic; not set from General, a private chat or a channel |
| acknowledgement | "Script succeeded!" to the sender | "Script succeeded!" to the origin chat and topic, silent |

A script that ignores `TELEGRAM_THREAD_ID` keeps working unchanged.
