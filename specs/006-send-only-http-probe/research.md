# Research: Send-Only Mode and HTTP Probe Checks

## R1 — How to stop polling without losing outbound messages

- **Decision**: swap the `app.TelegramConsumer` implementation for a no-op one when
  `BOT_TELEGRAM_POLL=false`.
- **Rationale**: `telegram.consumer` alone calls `getMe`, `bot.Start()` (`getUpdates`) and
  `publishMenu`, and alone consumes the `command_handler` group. The publisher sends through the
  same `*telebot.Bot` without starting it (`Offline: true` already avoids a `getMe` at
  construction). One switch in `appOptions` covers FR-002 entirely.
- **Alternatives**: a flag inside the consumer (more branches in tested code paths, and handlers
  still built); dropping `commandHandlers()` (it also provides shared adapters such as
  `command.Power`); a second bot token (rejected by the maintainer).

## R2 — Auth detection for an access proxy

- **Decision**: `auth_redirect` host pattern (approach A).
- **Rationale**: verified live: the target proxy answers a request with no service token, and one
  with a bogus token, with `302` to its login host (a different host from the app). The proxy's
  optional per-app "401 for service auth" mode is already covered by `auth_refusal`.
- **Alternatives**: B — built-in vendor detection: hidden behaviour, single vendor. C — 401/403
  only: depends on a per-app setting the target app does not use.
- **Residual risk**: a revoked or expired real token was not tested; the proxy is assumed to treat
  it like any invalid token.

## R3 — Host pattern syntax

- **Decision**: exact host, or `*.` + host meaning one or more labels before it;
  case-insensitive; ports ignored.
- **Rationale**: matches how login hosts are named (`<team>.<vendor-domain>`); avoids a glob
  library and `path.Match` quirks (`*` not crossing dots, `[` classes).
- **Alternatives**: `path.Match` (no multi-label match, odd metacharacters); regex (overkill, easy
  to under-anchor).

## R4 — Body read limit and judgement order

- **Decision**: read at most 64 KiB, only when a body pattern is set; judge auth → status → body.
- **Rationale**: health answers are tiny; a manifest is a few KiB. Not reading otherwise keeps
  today's cost. Status before body keeps one clear reason per alert.
- **Alternatives**: always read (needless traffic); judge body on any status (two reasons for one
  failure, confusing alerts).

## R5 — Legacy `header` vs `headers`

- **Decision**: keep both YAML keys; normalise `header` into `headers` in the loader; both set is
  an entry error.
- **Rationale**: SC-005 (old configs unchanged) with one code path in the checks.
- **Alternatives**: deprecate `header` with a warning (no benefit for a single maintainer; noise).
