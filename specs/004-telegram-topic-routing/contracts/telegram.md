# Contract: Telegram Behaviour

## Outgoing messages

Every message is sent to `TELEGRAM_CHAT_ID` with HTML parse mode, `message_thread_id` = the
resolved topic (omitted when 0) and `disable_notification` = silent.

| Message | Topic | Silent |
|---|---|---|
| PROBLEM | alerts | no |
| RESOLVED | alerts | yes |
| CHECKER ERROR | alerts | no |
| Checker recovered | alerts | yes |
| Fix action, memory-guard action, drive retired | alerts | no |
| Delivery-channel failure notice | alerts | no |
| A message carrying the "Undelivered earlier:" backlog | alerts | no |
| Incoming SMS (gammu inbox, modem) | sms | no |
| Weekly summary | system | yes |
| Command reply, usage text, error reply, "Script succeeded!" | origin chat and topic | yes |

Fall-back: when the send has a thread and Telegram answers with `message thread not found`,
`TOPIC_CLOSED` or `TOPIC_DELETED`, the same message is sent once more without
`message_thread_id`, silent unchanged, and a warning is logged. Any other failure is returned as
today.

## Incoming commands

| Update | Accepted when | Reply to |
|---|---|---|
| Channel post | channel id allowed | the channel, silent |
| Private message | chat id allowed | the private chat, silent |
| Group / supergroup message | chat id allowed **and** sender user id allowed | the group, the topic when `is_topic_message`, else General; silent |

`/name` and `/name@<this bot's username>` are the same command; `/name@<another bot>` is ignored.
A refused command produces no reply and one warning log with chat and sender id.

`/sms` additionally requires the origin chat to be `TELEGRAM_CHAT_ID`.
