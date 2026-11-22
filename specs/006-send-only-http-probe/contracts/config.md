# Contract: health YAML additions

## `http` entry

```yaml
http:
  - name: app
    url: https://app.example.org/health
    # Either header (one) or headers (list), never both.
    headers:
      - { name: X-Client-Id, value_env: APP_CLIENT_ID }
      - { name: X-Client-Secret, value_env: APP_CLIENT_SECRET, placeholders: [ "changeme" ] }
    expect_body: '"status"\s*:\s*"ok"'      # RE2; must match the first 64 KiB of the body
    forbid_body: 'maintenance'              # RE2; must not match
    auth_redirect: "*.login.example.net"    # a redirect to a matching host = refused credential
    auth_refusal: checker_error             # down (default) | checker_error
```

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `headers` | list of `{name, value_env, prefix, placeholders}` | none | every item sent; names unique (case-insensitive) |
| `header` | one item | none | unchanged; shorthand for a one-item `headers` |
| `expect_body` | RE2 string | none | body must match (judged only when status is in range) |
| `forbid_body` | RE2 string | none | body must not match |
| `auth_redirect` | `host` or `*.host` | none | `*.` matches one or more labels, not the bare domain; case-insensitive; must not match the URL host |

Messages (alert/reply text after `http <name>: `):

| Case | Text |
| --- | --- |
| body misses `expect_body` | `status 200; body does not match expect_body` |
| body matches `forbid_body` | `status 200; body matches forbid_body` |
| refused via redirect, `auth_refusal: down` | `redirected to the login page at <host>` |
| refused via redirect, `checker_error` | checker error `http/<name>/auth`: `endpoint redirected to the login page at <host>` |

Config errors (entry skipped, rest loaded): `header` and `headers` both set; empty `headers`;
duplicate header name; invalid regex; invalid `auth_redirect`; `auth_redirect` matching the URL
host.

## `freshness` entry (URL mode)

`headers` is accepted with the same rules as above, only together with `url`.
