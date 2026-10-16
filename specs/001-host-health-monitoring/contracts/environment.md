# Contract: Environment variables (new)

| Variable | Meaning | Default |
|---|---|---|
| `TELEGRAM_ALLOWED_IDS` | Comma-separated extra chat IDs allowed to issue commands (FR-044) | empty |
| `BOT_HEALTH_CONFIG` | Path to the health YAML; set ⇒ health monitoring enabled | unset |
| `BOT_HOST_NAME` | Prefix for every health message | `os.Hostname()` |
| `BOT_SMTP_ADDR` | `host:port` of an SMTP relay; set ⇒ checker errors also by email | unset |
| `BOT_SMTP_FROM` / `BOT_SMTP_TO` | Envelope + header addresses | required with `BOT_SMTP_ADDR` |
| `BOT_SMTP_USER` / `BOT_SMTP_PASS` | Optional PLAIN auth | unset |
| `BOT_SMS_PHONE` | Operator phone number for SMS alerts | required for `sms` modes |

Existing variables keep their meaning. Secrets referenced from the health config (`value_env`,
`phone_env`) are ordinary environment variables, named by the operator.
