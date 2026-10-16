# Quickstart: validating host health monitoring

Validation scenarios that prove the feature end to end. Contracts:
[config](contracts/config.md), [Telegram](contracts/telegram-commands.md),
[CLI](contracts/cli.md), [heartbeat](contracts/heartbeat.md), [env](contracts/environment.md).

## Prerequisites

- Linux host, tgshd built with `task build` (static, `CGO_ENABLED=0`).
- Root (SMART, `/dev/kmsg`, Docker socket).
- `smartmontools` installed; `nvidia-smi` only if an NVIDIA GPU is present.
- Optional: NUT server, Docker, gammu-smsd with the PostgreSQL backend, an SMTP relay.

## 1. Unit tests and coverage gate

```bash
task lint
task test:coverage     # gate scope now includes ./pkg/health/...
```

Expected: all green; coverage ≥ 80 % over the gate scope.

## 2. Access filter (FR-044)

1. Start tgshd with `TELEGRAM_CHAT_ID` set and no `TELEGRAM_ALLOWED_IDS`.
2. From a second Telegram account, open a private chat with the bot and send `/ping`.

Expected: no reply; a warn log line with that chat ID. The same command from the configured chat
works as before.

## 3. One-shot verification on a live host (SC-001)

```bash
sudo BOT_HEALTH_CONFIG=/etc/tgshd/health.yaml ./bin/tgshd check --once
```

Expected: one line per drive aspect, sensor, container, endpoint, file and probe, each `GOOD`,
`BAD` or `UNKNOWN` with text; exit 0; no Telegram/SMS/email sent; state DB unchanged
(`stat` mtime of `health.db` unchanged).

Compare by hand against `smartctl -n standby,3 -a /dev/<node>` (exit 3: the drive is asleep and
is skipped, not reported), `sensors`, `docker ps`, `curl -I` for a few entries.

## 4. Alert lifecycle (US1)

With a `freshness` entry `max_age: 2m` on a scratch file:

1. `date +%s > /tmp/stamp`, start the daemon → no message.
2. Wait > 2 min → exactly one `PROBLEM` (since time shown).
3. `systemctl restart tgshd` while broken → no second PROBLEM.
4. `date +%s > /tmp/stamp` → exactly one `RESOLVED`.
5. `/history freshness 24h` → both entries listed.

## 5. Quiet windows

1. `flock -x /run/maintenance.lock sleep 300 &`, break a check → no message during the 5 min;
   heartbeat `ts` keeps advancing (`cat /run/tgshd/heartbeat`).
2. After release, the check alerts only after its full threshold of *observed* time.

## 6. Heartbeat and planned reboot (FR-016/017)

```bash
sudo tgshd heartbeat --planned-reboot 900 && cat /run/tgshd/heartbeat   # planned_until ≈ now+900
sudo tgshd heartbeat --planned-reboot 0   && cat /run/tgshd/heartbeat   # planned_until=0
```

## 7. Network outage by SMS (SC-004)

Temporarily block the gateway (e.g. a firewall rule dropping ICMP to it) longer than the network
threshold. Expected: one SMS PROBLEM, no other alerts; after unblocking, one RESOLVED on Telegram.

## 8. Incoming SMS and modem probe (US4)

1. Remove `runonreceive` from the gammu-smsd configuration and restart gammu-smsd (otherwise
   messages are delivered twice).
2. Send an SMS to the modem → forwarded to Telegram once; its inbox row has `Processed = true`.
3. Set the probe schedule a few minutes ahead → probe sent, returns, `/status` stays green;
   `/history modem_probe` shows nothing (a pass is silent).

## 9. Hardware (US2)

- First start on an empty state dir → one message listing every drive.
- `/disks`, `/temp` reply with current values and 7-day statistics (statistics appear after
  samples accumulate).
- `/retire <serial>` for a drive that was removed → one "drive retired" message.

## 10. Read-only state (FR-013)

Mount the state dir read-only (`mount -o remount,ro` on a test filesystem) → one checker error
"cannot save state", alerts continue; remount rw → next tick saves, no duplicate alerts.

## Switch-over (operator, outside this repository)

After 3–10 succeed on the target host, disable the old cron health checkers and point the
external watchdog at the new heartbeat path.
