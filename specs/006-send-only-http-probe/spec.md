# Feature Specification: Send-Only Mode and HTTP Probe Checks

**Feature Branch**: `006-send-only-http-probe`

**Created**: 2026-11-22

**Status**: Draft

**Input**: User description: "Send-only Telegram mode and a more flexible http health check: a
second tgshd instance on a remote VM probes the home server's public hostnames from outside and
shares the bot token with the home instance; it must not poll for commands. The http check gains
several secret headers, a body match and detection of a redirect to a login page."

A second tgshd instance will run on a small remote VM as an outside-in probe of the home server.
Every few minutes it requests public hostnames whose traffic goes through a CDN edge, a tunnel and
a reverse proxy to each app, and it raises PROBLEM/RESOLVED in the Alerts topic. It uses the same
bot token as the home instance.

Two things stand in the way today:

- Both instances would long-poll Telegram for updates. Telegram allows one poller per token: it
  answers the second with a conflict, and commands reach either host at random.
- The `http` check judges only the status. An access proxy in front of an app answers an
  anonymous or wrongly-authenticated request with a redirect to its login page, a status inside
  the default good range, so the check cannot tell the app from the proxy. An app behind such a
  proxy is reached with a service token that takes two headers, and the check sends only one.

The probe must stay declarative (an `http` entry per endpoint), not a `command` check wrapping an
HTTP client.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Run a second instance that only sends (Priority: P1)

The maintainer starts a second tgshd on the remote VM with the shared bot token and send-only mode
switched on. It never asks Telegram for updates, so the home instance keeps receiving every
command without conflicts, and the remote instance still delivers its health alerts, weekly
summary and checker-error mail.

**Why this priority**: without it the second instance breaks command handling on the home server;
nothing else in this feature can be deployed safely first.

**Independent Test**: start an instance with send-only mode on and a health config with one
failing check; confirm it makes no update requests, publishes no command menu, and posts the alert
to the configured topic.

**Acceptance Scenarios**:

1. **Given** send-only mode is on, **When** the daemon runs, **Then** it never requests updates
   from Telegram and never changes the bot's command menu.
2. **Given** send-only mode is on and a check turns bad, **When** the alert is due, **Then** it is
   delivered to Telegram (in the configured topic), by SMS and by email exactly as in normal mode.
3. **Given** send-only mode is on, **When** someone sends a command to the bot, **Then** this
   instance does not answer it (only an instance that polls does).
4. **Given** the setting is absent, **When** the daemon runs, **Then** it polls and serves commands
   exactly as today.

---

### User Story 2 - Require the app's own health answer (Priority: P1)

The maintainer writes an `http` entry that requires the response body to match a pattern (the
app's own health answer, e.g. `"status":"OK"`, `pong`, `Service ready.`), and optionally to not
match another pattern. A response from anything else (a proxy's login redirect, an error page from
the edge, a default page) is judged down even when its status is in range.

**Why this priority**: it is what makes the outside-in probe meaningful; status alone reports
"up" while the app is unreachable behind the proxy.

**Independent Test**: point an entry at a test server that returns status 200 with a body that
does not match; confirm the condition turns bad and the alert names the entry without quoting the
body.

**Acceptance Scenarios**:

1. **Given** an entry with a required body pattern, **When** the response is in the status range
   and its body matches, **Then** the condition is good.
2. **Given** the same entry, **When** the response is in range but the body does not match,
   **Then** the condition is bad and the alert says the body did not match the expected pattern,
   without quoting the body.
3. **Given** an entry with a forbidden body pattern, **When** the body matches it, **Then** the
   condition is bad.
4. **Given** an entry with a body pattern, **When** the status is outside the range, **Then** the
   condition is bad for the status (the body is not judged).

---

### User Story 3 - Send several secret headers (Priority: P2)

The maintainer configures an `http` entry (or a `freshness` entry that reads its stamp over HTTP)
with a list of headers, each taking its value from its own environment variable, so a service
token made of a client id and a client secret can be presented.

**Why this priority**: needed for the one probed app behind the access proxy; the other probes
work without it.

**Independent Test**: configure two headers from two environment variables against a test server
that requires both; confirm the condition is good, and that a missing variable for either header
raises the existing checker error naming that variable.

**Acceptance Scenarios**:

1. **Given** an entry with two headers, **When** it runs, **Then** both are sent with their
   configured prefixes and environment values.
2. **Given** one header's variable is not set, empty or a placeholder, **When** the entry runs,
   **Then** no request is sent and a checker error names that variable, never a value.
3. **Given** an existing config with the single `header` form, **When** it is loaded, **Then** it
   behaves exactly as today.
4. **Given** redirects are followed, **When** a hop goes to a different scheme or host, **Then**
   none of the secret headers are sent to it.

---

### User Story 4 - Tell a refused credential from a down app (Priority: P2)

The maintainer sets `auth_redirect` to the access proxy's login host pattern. When the proxy
redirects the probe to its login page (because the service token expired, was revoked or is
wrong), the check reports a refused credential instead of a down app; with
`auth_refusal: checker_error` that is the checker error `http/<name>/auth`, not a PROBLEM alert.

**Why this priority**: an expired token otherwise looks exactly like the app being down.

**Independent Test**: point an entry at a test server that redirects to a host matching the
pattern; confirm the auth checker error is raised with `checker_error`, and a bad condition with
the default `down`.

**Acceptance Scenarios**:

1. **Given** `auth_redirect` matches and redirects are not followed, **When** the response is a
   redirect whose target host matches, **Then** it is a refused credential, judged by
   `auth_refusal`.
2. **Given** redirects are followed, **When** any hop would go to a matching host, **Then** that
   hop is not requested and the result is a refused credential.
3. **Given** a redirect to a host that does not match, **When** the entry runs, **Then** it is
   judged as today (status, then body).
4. **Given** a 401 or 403 answer, **When** the entry runs, **Then** it is a refused credential as
   today, whether or not `auth_redirect` is set.

---

### Edge Cases

- A body longer than the read limit: only the first 64 KiB are matched; the rest is ignored.
- The connection drops or the timeout runs out while the body is read: the endpoint is judged
  down with a transport-failure text, as for a failure before the answer.
- A body pattern that is not a valid regular expression: config error for that entry; the rest
  of the config stays usable.
- `header` and `headers` both set on one entry: config error for that entry.
- Two headers in the list with the same name (compared case-insensitively): config error.
- An empty `headers` list: config error (omit the key instead).
- `auth_redirect` that is not a host or `*.`-prefixed host pattern (a scheme, port, path or a
  bare `*`): config error.
- `auth_redirect` that matches the entry's own URL host: config error (every same-host redirect
  would look like a refused credential).
- A redirect with a relative `Location`: resolved against the request URL before matching, so it
  only matches when the entry's own host matches (which is rejected above).
- A redirect with no or an unparsable `Location`: not a refused credential; judged by status.
- Send-only mode with an unreadable value of the setting: treated like any other unparsable
  environment setting today (the daemon reports it at start).
- Send-only mode and the one-shot CLI subcommands (`check --once`, `heartbeat`): unaffected; they
  never poll.

## Requirements *(mandatory)*

### Functional Requirements

**Send-only mode**

- **FR-001**: The daemon MUST read a boolean environment setting `BOT_TELEGRAM_POLL`, default
  true.
- **FR-002**: With `BOT_TELEGRAM_POLL=false` the daemon MUST NOT request updates from Telegram,
  MUST NOT publish or clear the bot's command menu, and MUST NOT register command handlers.
- **FR-003**: In send-only mode every outbound feature MUST keep working: health alerts and
  recoveries, checker-error notices, the weekly summary, topic routing, SMS forwarding to
  Telegram, SMS and email delivery.
- **FR-004**: The daemon MUST log at start that it runs in send-only mode.

**Headers**

- **FR-005**: An `http` entry and a URL-mode `freshness` entry MUST accept `headers`, a non-empty
  list of items with the same fields and validation as today's `header` (`name`, `value_env`,
  `prefix`, `placeholders`).
- **FR-006**: The single `header` form MUST keep working unchanged; setting both `header` and
  `headers` on one entry MUST be a config error.
- **FR-007**: Header names in one entry MUST be unique, compared case-insensitively.
- **FR-008**: If any header's secret is unusable (not set, empty, control character, placeholder)
  the entry MUST NOT send a request and MUST report the existing checker error naming the
  variable; with several unusable secrets every variable is named.
- **FR-009**: When following redirects, the check MUST strip every configured header from any hop
  whose scheme or host differs from the configured URL.

**Body match**

- **FR-010**: An `http` entry MUST accept `expect_body` and `forbid_body`, each a regular
  expression in the same syntax as the `command` check's `expect_output` / `forbid_output`.
- **FR-011**: The body MUST be read up to 64 KiB; patterns are matched against that prefix.
- **FR-012**: The body is judged only when the status is inside the range; a body that fails
  `expect_body` or matches `forbid_body` MUST make the condition bad.
- **FR-013**: Alert, reply and checker-error texts MUST NOT include any part of the body; they say
  which pattern rule failed (expected / forbidden).
- **FR-014**: An invalid pattern MUST be a config error for that entry.

**Auth redirect**

- **FR-015**: An `http` entry MUST accept `auth_redirect`, a host name or a `*.`-prefixed host
  pattern (`*.example.org` matches any host ending in `.example.org`, not `example.org` itself),
  compared case-insensitively.
- **FR-016**: When redirects are not followed, a redirect response whose `Location` (resolved
  against the request URL) has a matching host MUST be judged a refused credential.
- **FR-017**: When redirects are followed, a hop to a matching host MUST NOT be requested, and the
  result MUST be judged a refused credential.
- **FR-018**: A refused credential detected this way MUST take the same verdict as a 401/403
  today: bad with the default `auth_refusal: down`, the checker error `http/<name>/auth` with
  `auth_refusal: checker_error`. The auth judgement comes before status and body.
- **FR-019**: A pattern that is not a valid host pattern, or that matches the entry's own URL
  host, MUST be a config error.

**Common**

- **FR-020**: Texts MUST NOT leak header values, the URL's query or userinfo, or the redirect
  target's query.
- **FR-021**: The example config MUST show `headers`, `expect_body`, `forbid_body` and
  `auth_redirect`, and the README MUST document `BOT_TELEGRAM_POLL`.

### Key Entities

- **Header item**: a header name, the environment variable holding its secret, an optional prefix,
  optional placeholder markers.
- **HTTP entry (extended)**: adds a header list, body patterns (expected, forbidden) and an
  auth-redirect host pattern to today's fields.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With the home instance polling and the remote instance in send-only mode on the same
  token, 100% of commands sent over a day are answered by the home instance and Telegram reports
  no update conflicts.
- **SC-002**: Every probed endpoint is expressed as an `http` entry; none needs a `command` check
  wrapping an HTTP client.
- **SC-003**: An endpoint answered by the access proxy's login redirect instead of the app is never
  reported as healthy.
- **SC-004**: An expired or wrong service token is reported as a credential problem, distinct from
  the app being down, within one check interval.
- **SC-005**: Every existing health config loads and behaves as before (no change in verdicts).

## Assumptions

- A refused service token at the access proxy answers with a redirect to a login host distinct from
  the app's host, or with 401/403 (verified for a bad token on the target proxy; a revoked or
  expired real token is assumed to behave like an invalid one).
- 64 KiB is enough to hold every health answer of interest; health endpoints answer with short
  bodies.
- The body match applies to the `http` check only; the `freshness` check already reads its body as
  a stamp.
- An unparsable `BOT_TELEGRAM_POLL` behaves like any unparsable environment setting today.
- No release tagging: the deploy builds from a commit on `main`.
- No new dependencies; no second bot token.
