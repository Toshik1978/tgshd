# Data Model: Telegram Topic Routing

All types below live in the pure package `pkg/telegram/route` unless noted.

## Destination

Where one Telegram message goes.

| Field | Type | Rules |
|---|---|---|
| ChatID | int64 | the chat; non-zero |
| ThreadID | int | topic thread id; 0 = no topic (General, or a chat without topics) |
| Silent | bool | true = sent without notification sound |

## Topic

`TopicNone` (zero), `TopicAlerts`, `TopicSMS`, `TopicSystem`.

## Route

What a sender asks for, before the chat is known.

| Field | Type | Rules |
|---|---|---|
| Topic | Topic | which of tgshd's topics |
| Silent | bool | sound |

Text form (persisted in `queue.route`): only the routes tgshd uses — `""` (zero: default for the
kind), `alerts`, `alerts/silent`, `sms`, `system/silent`. Any other value is a load error, like an
unknown queue kind today.

## Topics

The three settings, parsed once at startup by `ParseTopics(alerts, sms, system string)`.

| Input | Result | Warning |
|---|---|---|
| all three empty | every topic → thread 0 | none |
| a positive whole number | that topic → the number | none |
| empty while another is set | that topic → thread 0 | one, naming the setting |
| not a positive whole number | that topic → thread 0 | one, naming the setting |

`Topics.Resolve(chatID, Route) Destination`.

## Origin

Where an incoming command came from.

| Field | Type | Rules |
|---|---|---|
| ChatID | int64 | the chat the command was posted in |
| ThreadID | int | topic thread id when the message is a topic message, else 0 |
| SenderID | int64 | the user who sent it; 0 for channel posts |

`Origin.Reply() Destination` = `{ChatID, ThreadID, Silent: true}`.

## AccessList (moved from `pkg/telegram`)

Set of allowed ids: `TELEGRAM_CHAT_ID` ∪ `TELEGRAM_ALLOWED_IDS`.

`Permit(kind ChatKind, chatID, senderID int64) bool`:

| ChatKind | Permitted when |
|---|---|
| Channel | chat allowed |
| Private | chat allowed |
| Group (group, supergroup) | chat allowed **and** sender allowed |

## Default routes of health messages (`pkg/health/notify`)

`notify.Message.Route` and `notify.QueuedMessage.Route`; the zero value resolves by kind.

| Message | Kind | Route |
|---|---|---|
| PROBLEM | problem | alerts (default) |
| RESOLVED | resolved | alerts/silent (default) |
| CHECKER ERROR, fix, memory-guard, retired, delivery failure | event | alerts (default) |
| Checker recovered | event | alerts/silent (explicit) |
| Weekly summary | event | system/silent (explicit) |

A Telegram message carrying the "Undelivered earlier:" prefix is routed alerts, loud; the prefix
is attached only to messages whose route topic is alerts.

## Store change (`pkg/health/store`)

One additive migration: `ALTER TABLE queue ADD COLUMN route TEXT NOT NULL DEFAULT '';`. Read and
written with the other queue columns; validated against the text forms above.
