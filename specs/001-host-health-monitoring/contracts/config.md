# Contract: Health configuration file

Path: `$BOT_HEALTH_CONFIG`. If the variable is unset, health monitoring is off and tgshd behaves
as before. Strict YAML: unknown keys are errors. An invalid **entry** is reported as a checker
error (file, entry, reason) and skipped; a file that cannot be parsed at all disables health
monitoring with a logged error and a checker error.

Durations: Go syntax (`30s`, `15m`, `6h`) plus `d` for days (`8d`).

```yaml
# All top-level sections are optional; omitted ⇒ defaults; a check type absent ⇒ not run.
general:
  state_dir: /var/lib/tgshd          # DB, instance lock
  heartbeat_file: /run/tgshd/heartbeat
  maintenance_lock: /run/maintenance.lock   # optional; absent ⇒ no maintenance windows
  boot_grace: 10m
  concurrency: 4

summary:                              # weekly report; omit to disable
  weekday: monday
  time: "09:00"                       # host local time

network:                              # evaluated first every cycle
  interval: 5m
  threshold: 0                        # alert on first bad cycle (each cycle already pings 3×)
  internet_targets: [ "203.0.113.10", "198.51.100.20" ]   # any one answering ⇒ internet up
  mode: sms                           # PROBLEM by SMS; RESOLVED by tlg-or-sms (fixed)

ups:                                  # uses BOT_NUT_* connection settings
  interval: 5m
  threshold: 5m

disks:
  interval: 15m
  wear_percent: 80                    # NVMe percentage used / SSD wear above ⇒ PROBLEM
  selftest_max_age: 8d
  exclude_serials: []

temperatures:
  interval: 15m
  threshold: 15m
  limits:                             # °C, PROBLEM above
    cpu: 85
    drive_nvme: 70                    # judged by the disks check from SMART
    drive_sata: 55                    # judged by the disks check from SMART
    gpu: 85
    board: 75                         # chipset / board sensors

kernel:
  interval: 15m                       # EDAC counters; kmsg is read continuously

containers:
  interval: 5m
  threshold: 15m
  exclude: [ "runner-*" ]             # glob on container name

http:
  - name: web-app                     # subject in messages; [a-z0-9-]+
    url: https://app.example.org/health
    interval: 10m
    threshold: 10m
    timeout: 15s
    status: [200, 399]
    connect_to: 192.0.2.5             # optional: dial this address, keep URL host for TLS/Host
    doh: https://cloudflare-dns.com/dns-query   # optional, mutually exclusive with connect_to
    header: { name: Authorization, value_env: APP_TOKEN, prefix: "Bearer " }  # optional
    depends_on: http/other-app        # optional: skip while that condition is bad
    needs_internet: false
    container: app                    # optional: also require this container healthy

freshness:
  - name: db-dump
    path: /var/lib/backup-status/db
    content: epoch                    # epoch (file holds a Unix time) | mtime
    max_age: 26h

command:
  - name: port-guard
    run: [ "/usr/local/bin/probe-port" ]  # argv, no shell
    interval: 5m
    timeout: 60s
    threshold: 0
    fix:                              # optional
      after: 2                        # consecutive failures
      restart_container: worker       # or: run: [ "/usr/local/bin/fix" ]
      max_per_hour: 3
      grace_after_start: 2m
    report: journal                   # journal | alert (default alert)

modem_probe:                          # requires BOT_GAMMU_DSN
  phone_env: MODEM_PHONE              # env var holding the modem's own number
  weekday: sunday
  time: "11:02"
  reply_within: 30m
  max_silence: 8d
```

Per-check common keys: `interval`, `threshold`, `mode` (`tlg` | `sms` | `tlg-or-sms`, default
`tlg`), `needs_internet` (default `false`; `true` for `http` entries using `doh`).
