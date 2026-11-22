# Implementation Plan: Send-Only Mode and HTTP Probe Checks

**Branch**: `006-send-only-http-probe` (work lands on `main`) | **Date**: 2026-11-22 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `/specs/006-send-only-http-probe/spec.md`

## Summary

Four independent additions so a second tgshd can probe the home server from outside on the same
bot token:

1. **Send-only mode** — `BOT_TELEGRAM_POLL=false` swaps the Telegram consumer for a no-op one in
   the fx graph. The consumer is the only thing that calls `getMe`/`getUpdates` and publishes the
   menu, and the only consumer of the `command_handler` group, so none of that runs; the publisher
   and every worker stay wired.
2. **`headers:`** — a list of today's `Header` items on `http` and URL-mode `freshness`.
   The loader folds the legacy `header` into `headers`, so the checks see one list.
3. **`expect_body` / `forbid_body`** — RE2 patterns matched against the first 64 KiB of the body,
   judged only when the status is in range; the text names the failed rule, never the body.
4. **`auth_redirect`** — a host or `*.`-host pattern. A redirect to a matching host is a refused
   credential, judged by the existing `auth_refusal` verdict, before status and body.

**Approach (chosen in brainstorming) for auth detection.** **A — explicit `auth_redirect` host
pattern** feeding the existing `auth_refusal` verdict. Generic (any login proxy that redirects),
explicit in the YAML, independent of the proxy's per-app settings. Rejected: **B** built-in
detection of one CDN vendor's access-proxy redirect (vendor-specific hidden behaviour, no other
proxies); **C** rely on 401/403 only (depends on a per-app proxy setting; the target app
redirects today — verified live with no and with a bogus service token). Details:
[research.md](research.md).

## Technical Context

**Language/Version**: Go (`go.mod` toolchain)

**Primary Dependencies**: existing only — uber/fx, zap, telebot v4, caarlos0/env, yaml.v3,
`regexp`, `net/http`. **No new dependency.**

**Storage**: none.

**Testing**: standard library `testing`, table-driven; `httptest.Server` for the http check;
hand-written fakes (stamp source, env lookup).

**Target Platform**: Linux server (amd64), static `CGO_ENABLED=0` binary.

**Project Type**: single Go daemon.

**Performance Goals**: none new; at most 64 KiB read per http run, only when a body pattern is
set.

**Constraints**: no secret, URL query/userinfo, redirect query or body text in any message
(constitution, Engineering Constraints); old configs load unchanged (SC-005).

**Scale/Scope**: a handful of probe entries every ~5 min.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | Status | Notes |
| --- | --- | --- |
| I. Idiomatic Go, stdlib first | ✅ | `regexp`, `net/http`, `strings`; follows `command` check patterns (`compilePatterns`, `validateOutputPattern`) |
| II. Test-first, stdlib testing | ✅ | All decision logic in `pkg/health/config`, `pkg/health/check/httpcheck`, `pkg/health/check/freshness` (in `COVERPKG`); the fx switch in `cmd` stays a one-line choice, tested in `cmd` like `appOptions` today |
| III. Dependency discipline | ✅ | none added |
| IV. CGO-free | ✅ | nothing new |
| V. Env-gated, always starts | ✅ | send-only is an env switch in `appOptions`; Telegram outages still never stop the daemon |
| VI. Public repo hygiene | ✅ | examples use `example.org`; no real hosts in spec, tests, docs |
| VII. Commits | ✅ | Conventional Commits on `main`, no push without request |

Post-design re-check: unchanged, all ✅.

## Design

### Send-only (`cmd`, `pkg/telegram`)

- `config.TelegramPoll bool \`env:"BOT_TELEGRAM_POLL" envDefault:"true"\``.
- `appOptions(getenv)` reads `BOT_TELEGRAM_POLL` with `strconv.ParseBool`; when it parses to
  `false` it provides `telegram.NewSendOnlyConsumer` as `app.TelegramConsumer` instead of
  `telegram.NewConsumer`. An unparsable value keeps the polling wiring and `newConfig` fails the
  start with the env parse error, as for any bad env value today.
- `NewSendOnlyConsumer(logger)`: `Start` logs "Telegram send-only mode: no command polling" and
  returns; `Stop` returns. Unit-tested in `pkg/telegram`.
- `commandHandlers()` stays wired (other providers such as `command.Power` are shared); fx never
  builds group members nobody consumes, so no handler is constructed.

### Config (`pkg/health/config`)

- `HTTP` gains `Headers []Header \`yaml:"headers"\``, `ExpectBody`, `ForbidBody`,
  `AuthRedirect string`. `Freshness` gains `Headers`.
- Validation: `header` and `headers` both set → error; empty `headers` → error; each item
  validated by today's `validateHeader` (labels `headers[i].name`); duplicate names
  (case-insensitive) → error; body patterns by `validateOutputPattern`; `auth_redirect` by
  `validHostPattern` and `!MatchHost(pattern, urlHost)`.
- After validation the loader normalises: `Headers = []Header{*Header}; Header = nil`, so every
  consumer reads `Headers` only.
- `MatchHost(pattern, host string) bool` (exported; used by validation and the check):
  case-insensitive; `*.example.org` matches `a.example.org`, `a.b.example.org`, not
  `example.org`; otherwise exact. Port is stripped from `host` first.

### HTTP check (`pkg/health/check/httpcheck`)

- `HeaderSecrets(hs []config.Header, lookupEnv) (http.Header, error)` replaces `HeaderSecret`;
  every unusable secret is named, errors joined, all wrapping `ErrSecret`. Shared with freshness.
- `checkRedirect` strips every configured header name on a cross-scheme/host hop, and returns
  `errAuthRedirect` for a hop whose host matches `auth_redirect` (hop not requested).
- `probe` order: transport error (`errors.Is(err, errAuthRedirect)` → auth outcome) → 401/403 →
  unfollowed 3xx whose `resp.Location()` host matches → auth outcome → status range → body.
- Auth outcome (`authRefused(text)`): with `checker_error` the existing `CheckError{Kind: auth}`;
  otherwise Bad with text "redirected to the login page at <host>" (host only).
- Body: read only when a pattern is set, `io.LimitReader(resp.Body, 64<<10)`; a read error is Bad
  via `describe`; texts "body does not match expect_body" / "body matches forbid_body".
  Patterns compiled in `New` (an invalid one is a start error, though config validation already
  rejects it).

### Freshness (`pkg/health/check/freshness`, `pkg/host/httpstamp`)

- `StampSource.Fetch(ctx, url string, header http.Header)`; `httpstamp.Client.Fetch` sets each.
  Freshness never follows redirects, so no stripping is needed.

### Docs

- `config/health.example.yaml`: `headers`, `expect_body`, `forbid_body`, `auth_redirect`
  examples; README env table gains `BOT_TELEGRAM_POLL`; README http-check table gains the keys.
- `.claude/CLAUDE.md` package map unchanged (no new package).

## Project Structure

### Documentation (this feature)

```text
specs/006-send-only-http-probe/
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── contracts/
│   ├── config.md        # health YAML keys
│   └── env.md           # BOT_TELEGRAM_POLL
└── tasks.md             # /speckit-tasks
```

### Source Code (repository root)

```text
cmd/
├── main.go                         # TelegramPoll field; appOptions consumer switch
└── main_test.go                    # appOptions wiring with BOT_TELEGRAM_POLL=false
pkg/telegram/
├── sendonly.go (+ _test)           # no-op consumer
pkg/health/config/
├── checks.go                       # Headers, ExpectBody, ForbidBody, AuthRedirect
├── validate.go (+ _test)           # header list, body patterns, auth_redirect
├── config.go (+ _test)             # header → headers normalisation
└── hostpattern.go (+ _test)        # MatchHost, validHostPattern
pkg/health/check/httpcheck/
├── secret.go (+ _test)             # HeaderSecrets
├── httpcheck.go (+ _test)          # headers, body match, auth redirect
pkg/health/check/freshness/stamp.go (+ _test)
pkg/host/httpstamp/httpstamp.go (+ _test)
config/health.example.yaml
README.md
```

**Structure Decision**: single Go daemon; changes stay in the existing packages above, plus one
small file each in `pkg/telegram` and `pkg/health/config`.

## Complexity Tracking

None.
