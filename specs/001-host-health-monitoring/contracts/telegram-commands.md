# Contract: Telegram commands

Accepted only from chats in `TELEGRAM_CHAT_ID` ∪ `TELEGRAM_ALLOWED_IDS` (FR-044). Updates from
any other chat are dropped without a reply and logged with chat and sender IDs. Replies use HTML
mode; every reply fits one message (≤ 4 096 chars, truncated with `…`).

| Command | Args | Reply |
|---|---|---|
| `/status` | — | Open problems, one per line: `<key>: <text> (since <time>)`; or `All checks OK (<n> checks, last cycle <time>)`. Notes a degraded store or quiet window if active. |
| `/history` | `[check] [period]` — check name or condition key prefix; period `24h`, `7d`, `30d` (default `7d`) | Alert log newest first: `<time> <KIND> <key>: <text>`; `No alerts in <period>` when empty. |
| `/temp` | — | Per sensor: `<label>: <now>°C (7d min/avg/max a/b/c)`. |
| `/disks` | — | Per drive: `<bus> <model> …<last 4 of serial>: health <OK/FAILING>, wear <n>% (7d <min>/<avg>/<max>), spare <n>%, temp <n>°C (7d <min>/<avg>/<max>), written 7d <size>, self-test <age>`. |
| `/retire` | `<serial or last 4+ chars>` | `Retired <drive>; cleared <n> conditions.` Ambiguous/unknown ⇒ list of known drives. |
| `/clear` | `<condition key>` | `Cleared <key> (no RESOLVED sent).` Unknown ⇒ list of open keys. |

Alert messages (pushed, not replies):

```text
<host>: PROBLEM: <text> (since HH:MM)
<host>: PROBLEM: <text> (since DD Mon HH:MM) # streak began on an earlier host-local day
<host>: RESOLVED: <text>
<host>: <event text>                      # counter rise, drive announce, retire, fix action
<host>: CHECKER ERROR: <check>: <text>    # at most once per 24 h per key
```

The since time is in the host's zone; it carries the date (e.g. `since 02 Jan 15:04`) when the
streak began on an earlier day, as for a stale self-test, a 26 h freshness limit or days of modem
silence.
