---

description: "Task list for send-only mode and HTTP probe checks"
---

# Tasks: Send-Only Mode and HTTP Probe Checks

**Input**: Design documents from `/specs/006-send-only-http-probe/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md

**Tests**: Every task is implemented test-first (Constitution II): each task writes its own tests,
standard library `testing` only, table-driven with hand-written fakes and `httptest` servers.
There are no standalone test tasks.

**Public repository**: tests, docs and examples use `example.org` / `example.net` hosts and
placeholder variable names only; no real host, app or URL.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel (different files, no dependency on an incomplete task)
- **[Story]**: US1–US4 from spec.md

---

## Phase 1: User Story 1 — Run a second instance that only sends (Priority: P1) 🎯 MVP

**Goal**: `BOT_TELEGRAM_POLL=false` runs no command poller, no menu publish, no command handlers;
every outbound message still goes out.

**Independent Test**: `appOptions` with `BOT_TELEGRAM_POLL=false` builds an fx graph whose
`app.TelegramConsumer` is the send-only consumer and constructs no `command_handler`; quickstart.md
manual step 1.

- [x] T001 [P] [US1] Add `NewSendOnlyConsumer(logger *zap.Logger)` in pkg/telegram/sendonly.go returning a type that satisfies `app.TelegramConsumer`: `Start(ctx)` logs one info line "Telegram send-only mode: no command polling" and returns nil without any Telegram call; `Stop(ctx)` returns nil; tests in pkg/telegram/sendonly_test.go with a zap observer assert the log line and nil errors (FR-002, FR-004, R1)
- [x] T002 [US1] In cmd/main.go add `TelegramPoll bool \`env:"BOT_TELEGRAM_POLL" envDefault:"true"\`` to `config`, and in `appOptions(getenv)` parse `getenv("BOT_TELEGRAM_POLL")` with `strconv.ParseBool`: when it parses to `false`, provide `telegram.NewSendOnlyConsumer` as `app.TelegramConsumer` instead of `telegram.NewConsumer` (split `configuration()` so the consumer provider is chosen separately; the publisher, `newTelegramBot`, `newAccessList`, `newTopics` and `commandHandlers()` stay wired); an empty or unparsable value keeps the polling consumer ("Another value fails the start with the environment parse error, like any other malformed `BOT_*` value" — `newConfig` reports it); tests in cmd/main_test.go: with `false` the graph validates, the resolved consumer is the send-only type and a probe handler provided into `group:"command_handler"` is never constructed; with unset and `true` the polling consumer is used; with `nope` `newConfig` fails (FR-001, FR-002, FR-003, contracts/env.md, R1)

**Checkpoint**: a send-only instance starts, logs its mode, and still delivers health messages through the publisher.

---

## Phase 2: User Story 2 — Require the app's own health answer (Priority: P1)

**Goal**: `expect_body` / `forbid_body` on `http` entries, judged on the first 64 KiB when the
status is in range; texts never quote the body.

**Independent Test**: httptest server answering 200 with matching / non-matching / forbidden
bodies and a 500 with a matching body; config entries with invalid patterns are skipped.

- [x] T003 [US2] In pkg/health/config/checks.go add `ExpectBody string \`yaml:"expect_body"\`` and `ForbidBody string \`yaml:"forbid_body"\`` to `HTTP` (comment: "must match / must not match the first 64 KiB of the body (Go RE2)"); in pkg/health/config/validate.go `validateHTTP` call `validateOutputPattern(p, "expect_body", h.ExpectBody)` and `validateOutputPattern(p, "forbid_body", h.ForbidBody)`; tests in pkg/health/config/config_test.go: valid patterns load, an invalid regex in either key is an entry error and the other entries still load (FR-010, FR-014)
- [x] T004 [US2] In pkg/health/check/httpcheck/httpcheck.go compile `ExpectBody`/`ForbidBody` in `New` (an invalid pattern is a start error naming the key, never the url); in `probe`, after the status-range judgement passes and only when a pattern is set, read `io.LimitReader(resp.Body, maxBody)` with `maxBody = 64 << 10` before closing it; a read error is Bad with `describe(err, within)`; `expect_body` not matching → Bad "status N; body does not match expect_body"; `forbid_body` matching → Bad "status N; body matches forbid_body"; out-of-range status is judged by status only and the body is not read; no body text in any outcome; update the package doc comment; tests in pkg/health/check/httpcheck/httpcheck_test.go with httptest servers: match → Good, miss → Bad, forbidden → Bad, 500 with matching body → Bad for status, match only past 64 KiB → Bad, body cut by the server mid-read → Bad, and a text assertion that a secret-looking body string never appears in the reading (FR-011, FR-012, FR-013, R4)

**Checkpoint**: an `http` entry can require the app's own answer.

---

## Phase 3: User Story 3 — Send several secret headers (Priority: P2)

**Goal**: `headers:` list on `http` and URL-mode `freshness`; `header` keeps working; every secret
header is stripped on cross-host redirects.

**Independent Test**: an httptest server requiring two headers; one unset variable → checker error
naming it; legacy `header` configs load to the same one-item list.

- [x] T005 [US3] In pkg/health/config/checks.go add `Headers []Header \`yaml:"headers"\`` to `HTTP` and `Freshness` (comment that `Header` is folded into it at load); in pkg/health/config/validate.go: `header` and `headers` both set → "header and headers are mutually exclusive"; `headers` present but empty → "headers must not be empty"; each item validated by `validateHeader` with labels `headers[i].name`, `headers[i].value_env`, `headers[i].prefix`; duplicate names compared case-insensitively → "headers[i].name %q repeats an earlier header"; for freshness, `headers` without `url` → "headers is only valid with url"; after a successful validation set `Headers = []Header{*Header}` and `Header = nil` (in the http and freshness validate functions or right after decode in config.go, whichever keeps `validateHeader` labels intact); tests in pkg/health/config/config_test.go: legacy `header` → one-item `Headers` and nil `Header`, two headers load, both keys → entry error, empty list, duplicate (`X-A` vs `x-a`), invalid item, freshness `headers` without `url` (FR-005, FR-006, FR-007, R5)
- [x] T006 [US3] In pkg/health/check/httpcheck/secret.go replace `HeaderSecret` with `HeaderSecrets(hs []config.Header, lookupEnv func(string) (string, bool)) (http.Header, error)`: nil/empty → empty header, nil error; each item judged as today (not set, empty, control character, placeholder); every unusable item contributes one error "environment variable NAME <reason>" and the errors are joined, each wrapping `ErrSecret`, never a value; usable items set `prefix+value` under their name; in pkg/health/check/httpcheck/httpcheck.go `Check` keeps `headers []config.Header`, `Run` calls `HeaderSecrets`, `probe` sets every header, `checkOptions` rejects a control character in any prefix, `checkRedirect` deletes every configured header name on a hop whose scheme or host differs from the first request; tests in pkg/health/check/httpcheck/*_test.go: two headers reach the server, two unset variables are both named in `Err`, a cross-host redirect hop receives none of the headers while a same-host hop receives all (FR-008, FR-009, FR-020)
- [x] T007 [US3] Change `freshness.StampSource.Fetch` to `Fetch(ctx context.Context, stampURL string, header http.Header) (status int, body []byte, err error)` in pkg/health/check/freshness/stamp.go and have `httpStamp` build the header with `httpcheck.HeaderSecrets(c.cfg.Headers, nil)` (an error stays `c.unreadable(err)`); update pkg/host/httpstamp/httpstamp.go `Client.Fetch` to set every header (`req.Header[k] = v` for each) and its callers in cmd; update fakes and tests in pkg/health/check/freshness/*_test.go and pkg/host/httpstamp/httpstamp_test.go: two headers reach the fake/httptest server, an unusable secret is a checker error naming the variable (FR-005, FR-008)

**Checkpoint**: an endpoint needing a two-header service token is probed; old configs behave as before.

---

## Phase 4: User Story 4 — Tell a refused credential from a down app (Priority: P2)

**Goal**: `auth_redirect` host pattern; a redirect to a matching host is a refused credential,
judged by `auth_refusal`, before status and body.

**Independent Test**: httptest servers redirecting to a matching and a non-matching host, with
redirects followed and not followed, under both `auth_refusal` values.

- [ ] T008 [P] [US4] Add pkg/health/config/hostpattern.go with `MatchHost(pattern, host string) bool` and `validHostPattern(pattern string) bool`: "case-insensitive; `*.example.org` matches `a.example.org`, `a.b.example.org`, not `example.org`; otherwise exact"; "Port is stripped from `host` first"; a valid pattern is a host name of letters, digits, hyphens and dots, optionally prefixed by `*.`, with no scheme, port, path, `*` elsewhere, empty label or bare `*`; table tests in pkg/health/config/hostpattern_test.go (FR-015, R3)
- [ ] T009 [US4] In pkg/health/config/checks.go add `AuthRedirect string \`yaml:"auth_redirect"\`` to `HTTP`; in pkg/health/config/validate.go `validateHTTP`: a non-empty value failing `validHostPattern` → "auth_redirect %q must be a host or *.host pattern"; a value matching the URL's own host (`MatchHost(pattern, u.Hostname())`) → "auth_redirect %q matches the url host itself"; tests in pkg/health/config/config_test.go for valid, invalid and self-matching values (FR-019)
- [ ] T010 [US4] In pkg/health/check/httpcheck/httpcheck.go: add `errAuthRedirect`; `checkRedirect` returns it (before the header strip and redirect cap) when `AuthRedirect` is set and `config.MatchHost(AuthRedirect, req.URL.Host)`, so that hop is not requested; in `probe`, a client error wrapping `errAuthRedirect` → refused credential; after the 401/403 branch, an unfollowed 3xx whose `resp.Location()` parses and whose host matches → refused credential; a missing or unparsable `Location` is not a refused credential; refused credential: with `auth_refusal: checker_error` the existing `health.CheckError{Kind: kindAuth}` with "endpoint redirected to the login page at <host>", otherwise Bad "redirected to the login page at <host>" (host only, never path or query); the auth judgement precedes status and body; update the package doc comment; tests in pkg/health/check/httpcheck/httpcheck_test.go with two httptest servers (app and a fake login host reached through the test dialer): unfollowed redirect to matching host under `down` → Bad and under `checker_error` → auth checker error; followed chain whose second hop matches → refused and the login server receives no request; redirect to a non-matching host → judged by status; relative `Location` → not refused; 401 still refused with `auth_redirect` set; reading texts contain no query string (FR-016, FR-017, FR-018, FR-020, R2)

**Checkpoint**: an expired or wrong service token raises `http/<name>/auth` instead of a PROBLEM.

---

## Phase 5: Polish & Cross-Cutting Concerns

- [ ] T011 Update config/health.example.yaml (an `http` entry showing `headers` with two items, `expect_body`, `forbid_body`, `auth_redirect` with `auth_refusal: checker_error`, and a note that `header` is a one-item shorthand exclusive with `headers`; the freshness URL example notes `headers`) and README.md (env table row for `BOT_TELEGRAM_POLL` per contracts/env.md with a short "Second instance on the same bot" paragraph; http-check key table rows for `headers`, `expect_body`, `forbid_body`, `auth_redirect`; freshness `headers`), using `example.org` hosts only; extend the existing example-config load test (pkg/health/config tests that load config/health.example.yaml) so the new keys are asserted to load without entry errors (FR-021)

---

## Dependencies & Execution Order

- Phases are independent user stories and can land in any order; the listed order is priority order.
- Within Phase 1: T001 → T002. Within Phase 2: T003 → T004. Within Phase 3: T005 → T006 → T007.
  Within Phase 4: T008 → T009 → T010.
- Phase 5 after Phases 2–4 (documents their keys).

## Parallel Opportunities

- T001 and T008 touch new files only and can start at once.
- Phases 1 and 2–4 touch disjoint packages (`cmd`/`pkg/telegram` vs `pkg/health/...`).
- Phases 2, 3 and 4 all edit `httpcheck.go` and `validate.go`; run them sequentially.

## Implementation Strategy

1. Phase 1 (send-only) first: it is the blocker for deploying a second instance at all.
2. Phase 2 (body match): makes the probe meaningful.
3. Phases 3–4: the one endpoint behind the access proxy.
4. Phase 5, then `task lint`, `task test:coverage`, `task build`.
