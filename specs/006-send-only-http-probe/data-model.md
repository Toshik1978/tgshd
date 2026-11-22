# Data Model: Send-Only Mode and HTTP Probe Checks

## Header (unchanged shape)

| Field | YAML | Rules |
| --- | --- | --- |
| Name | `name` | required; RFC 7230 token |
| ValueEnv | `value_env` | required; secret read from this environment variable at each run |
| Prefix | `prefix` | optional; no control characters except tab |
| Placeholders | `placeholders` | optional markers; a value containing one (case-insensitive) is unusable |

## HTTP entry (additions)

| Field | YAML | Rules |
| --- | --- | --- |
| Headers | `headers` | non-empty list of Header; names unique case-insensitively; exclusive with `header` |
| ExpectBody | `expect_body` | optional RE2 pattern; must match the first 64 KiB of the body |
| ForbidBody | `forbid_body` | optional RE2 pattern; must not match |
| AuthRedirect | `auth_redirect` | optional host or `*.host`; must not match the URL's own host |

After loading, `Header` is folded into `Headers` and cleared.

## Freshness entry (additions)

| Field | YAML | Rules |
| --- | --- | --- |
| Headers | `headers` | as for HTTP; only valid with `url`; exclusive with `header` |

## Daemon configuration (addition)

| Field | Env | Default | Rules |
| --- | --- | --- | --- |
| TelegramPoll | `BOT_TELEGRAM_POLL` | `true` | boolean (`strconv.ParseBool`); `false` = send-only |

## HTTP run judgement (order)

1. Header secrets unusable → checker error (no request).
2. Transport failure → Bad (or unjudged per today's DoH/cancel rules); a followed hop to an
   `auth_redirect` host → refused credential.
3. 401/403 → refused credential.
4. Unfollowed 3xx whose `Location` host matches `auth_redirect` → refused credential.
5. Status outside range → Bad.
6. Body pattern fails (or body read fails) → Bad.
7. Otherwise Good. Container aspect merged as today.

Refused credential: `auth_refusal: down` → Bad; `checker_error` → checker error
`http/<name>/auth`.
