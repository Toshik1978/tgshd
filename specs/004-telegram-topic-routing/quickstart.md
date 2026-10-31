# Quickstart: Validating Telegram Topic Routing

## Automated

```bash
task lint            # 0 issues
task test:coverage   # race on, 80 % floor over COVERPKG (now incl. ./pkg/telegram/route)
CGO_ENABLED=0 task build
```

Key test areas: `pkg/telegram/route` (topic parsing and warnings, resolve, origin reply, access
decision per chat kind, thread-rejection fall-back), `pkg/command` (each handler replies to the
origin, `/sms` origin-chat rule, script thread id), `pkg/sms` (forwards to the SMS destination,
loud), `pkg/health/notify` (default and explicit routes, backlog only on alerts and loud, queued
retry keeps its route), `pkg/health/store` (route column round-trip, old rows load with an empty
route), `pkg/script` (`TELEGRAM_THREAD_ID` present only when a thread is given).

## Manual, against a test forum supergroup

Prerequisites: a test bot that is an administrator of a test forum supergroup with three topics;
your user id in `TELEGRAM_ALLOWED_IDS`; `TELEGRAM_CHAT_ID` = the group; the three
`TELEGRAM_TOPIC_*` set to the topics' thread ids (read them from a topic link: the number after
the group part).

1. Start tgshd with a health config whose one check is forced to fail; expect the PROBLEM in the
   alerts topic with sound, then (after fixing it) RESOLVED in the same topic without sound.
2. Send `/status` in the SMS topic, in General, and `/status@<bot>` in the alerts topic; each reply
   lands where the command was typed, without sound.
3. Reply with `/status` to a message in General; the answer lands in General.
4. From a second account in the group, send `/status`; nothing is answered, one warning is logged.
5. Run a custom-script command in a topic with a script that prints `TELEGRAM_THREAD_ID` to a
   file; the file holds the thread id. From General it is unset.
6. Set `TELEGRAM_TOPIC_ALERTS=abc`, restart: one startup warning; a PROBLEM lands in General with
   sound.
7. Close the alerts topic in the client, trigger a PROBLEM: it lands in General, a warning names
   the thread.
8. Unset all three topic settings: no routing warning; RESOLVED and replies still arrive without
   sound.
