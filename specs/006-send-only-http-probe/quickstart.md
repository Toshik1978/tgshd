# Quickstart: validating Send-Only Mode and HTTP Probe Checks

## Automated

```bash
task lint
task test:coverage   # race detector + 80% gate
task build
```

Key tests: `pkg/telegram` (send-only consumer), `cmd` (`appOptions` with
`BOT_TELEGRAM_POLL=false`), `pkg/health/config` (headers, body patterns, auth_redirect validation
and normalisation, `MatchHost`), `pkg/health/check/httpcheck` (httptest servers for each
judgement step in [data-model.md](data-model.md)), `pkg/health/check/freshness`,
`pkg/host/httpstamp`.

## Manual

1. **Send-only**: run `bin/tgshd` with `BOT_TELEGRAM_POLL=false`, the bot token and a health YAML
   with one failing `http` entry. Expect the log line about send-only mode, a PROBLEM in the
   Alerts topic, and no `getUpdates` (the polling instance elsewhere gets no 409 Conflict). Send
   `/status`: only the polling instance answers.
2. **Body match**: an entry with `expect_body` against an endpoint that answers 200 with a
   different body → PROBLEM `body does not match expect_body`; fix the pattern → RESOLVED.
3. **Headers + auth redirect**: an entry behind an access proxy with two `headers` from env and
   `auth_redirect` set to the proxy's login host pattern, `auth_refusal: checker_error`. With
   valid secrets → good. With a wrong secret → checker error `http/<name>/auth`, no PROBLEM. With
   one variable unset → checker error naming that variable.
4. **Old config**: start with the existing production health YAML; verdicts unchanged.

Contracts: [contracts/config.md](contracts/config.md), [contracts/env.md](contracts/env.md).
