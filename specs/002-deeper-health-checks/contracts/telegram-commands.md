# Contract delta: Telegram commands, pushed messages and the weekly summary

**Amends**: [001 contracts/telegram-commands.md](../../001-host-health-monitoring/contracts/telegram-commands.md).
Access rules, HTML mode and the one-message limit (4 096 UTF-16 units, truncated with `…`) are
unchanged. `/history`, `/retire` and `/clear` are unchanged.

## `/status` (amended)

Unchanged format, plus: the quiet note reflects the configuration and lists the checks the
window pauses (those with `pause_in_quiet_windows`), so the operator sees which checks are still
judged (FR-026, FR-027, R15):

```text
Note: quiet window active (maintenance, held since <time>); paused: <check>, <check>, …
Note: quiet window active (boot grace); paused: <check>, <check>, …
Note: quiet window active (maintenance, held since <time>); paused: none
```

Every check not listed keeps running during the window.

## `/disks` (amended; FR-003, FR-004, FR-009, FR-012)

One line per active drive:

```text
<node> <bus> <model> …<last 4 of serial>: health <OK|FAILING|unknown|missing>, wear <n>% (7d a/b/c), spare <n>% (floor <f>%, drive <t>%), temp <n>°C (7d a/b/c), written 7d <size>, self-test <result> <age>, last read <age> ago[, asleep since <time>][, unreadable since <time>]
```

- `wear` covers NVMe percentage used and SATA SSD wear (100 − attribute 177, only when smartctl names it `Wear_Leveling_Count`).
- `spare` appears for NVMe drives only.
- `self-test` reads `passed 12 d ago`, `failed 3 d ago`, `none (first seen 5 d ago)`, or
  `not readable` for a drive whose log format is not supported (always for SAS/SCSI drives).
- `health unknown`: the last read had no health verdict.
- `last read` is the time since the last successful read (FR-003); a drive read in the last run
  shows `last read <1 m ago`.

## `/temp` (amended; FR-016, FR-017, FR-018)

```text
<sensor>: <now>°C (7d min/avg/max a/b/c)[, limit <n>°C]
<sensor>: unreadable since <time>
```

- CPU: one line per package (`coretemp package-id-0`, `k10temp tdie`), never per core.
- Board sensors: listed ones with their limit; unlisted ones show their critical limit when they
  expose one, else no limit.
- GPUs as before; a GPU that stopped reporting shows `unreadable since <time>`.

## Weekly summary (amended; FR-044 – FR-049)

One pushed event, at most one message:

```text
Weekly summary
Drives:
sda (/, /home) 41°C, self-test passed 12 d ago, wear 8%
sdb (-) asleep since Tue 14:00, self-test passed 30 d ago
nvme0n1 (/var/lib) 48°C, self-test passed 2 d ago, wear 3%
sdc (?) unreadable since Mon 09:10, self-test not readable
Services:
network: ok
ups: ok
containers: ok
web: ok
http/api: status 502, expected 200-399
backups: db-dump 30 h ago; offsite-copy <1 h ago
command/port-guard: ok
modem_probe: last round trip 3 d ago, no probe outstanding
Open problems:
http/api: status 502, expected 200-399 (since 08:12)
```

- Drive line: node, mount points of the drive, its partitions and their md/dm holders in
  parentheses (`-` none, `?` unknown), temperature, newest self-test result and the calendar age
  of the last passed test, wear when known; `asleep since` / `unreadable since` / `missing` when
  the last attempt did not read it.
- Services: one line per configured check, or per `summary_group`; `ok` or the open problems'
  texts joined with `; `. Freshness checks show their stamp age (`<1 h`, `N h` below 48 h, `N d`,
  `in the future` for a stamp dated after now, `none` when missing, `?` when unreadable),
  prefixed by the check name inside a group. The modem probe shows `last round trip <age> ago` or `never`, or its failure text, and whether a probe is
  outstanding.
- Cut order when too long: services first (ok lines fold into `… and N more ok`, then
  `… N more`), then drives (`… N more drives`), then open problems with a trailing `…`. The
  open-problems section is never dropped.

## Pushed messages (amended)

```text
<host>: PROBLEM: <text> (since HH:MM)
<host>: RESOLVED: <text>
<host>: <event text>
<host>: CHECKER ERROR: <check>: <text>        # at most once per 24 h per <check>/<subject>/<kind> (FR-028); <check> is the whole check name, e.g. command/<name>
<host>: CHECKER OK: health: state can be saved again       # once, after a reported save failure (FR-032)
```

New and changed texts:

| Source | Text | FR |
|---|---|---|
| network | `PROBLEM: No usable default route` / `Router <addr> is not answering` / `Internet is unreachable (router <addr> answers)`; one PROBLEM and one RESOLVED (`Network is reachable`) per outage; PROBLEM by SMS | FR-033, FR-034 |
| disks | `drive <serial> (<model>): <counter name> (ATA <id>) rose <a>→<b>` for every built-in or added counter, e.g. `reallocated sectors (ATA 5) rose 0→1`; the counter name itself never embeds the id, and an added id without a known name reads `attribute <id> (ATA <id>)`; NVMe: `media errors (NVMe) rose <a>→<b>` | FR-006 |
| disks | `drive <serial> (<model>): available spare <n>% at or below its threshold <t>%` when the spare is at or below the drive's own threshold; otherwise `available spare <n>% below the <f>% floor` | FR-008 |
| disks | `drive <serial> (<model>): self-test failed (<description>) at <h> h; a newer test has passed since` | FR-010 |
| disks | `drive <serial> (<model>): last passed self-test <n> d ago, limit <m> d` | FR-011 |
| kernel | `Kernel (previous boot): <class> found (<n> records). Last: <line>` | FR-020 |
| kernel | `Kernel: uncorrectable memory errors on <mc> rose <a>→<b>` (every rise) | FR-024 |
| kernel | `Kernel: <class> still happening (<n> records since <time>). Last: <line>` (also without a new record) | FR-022 |
| temps | `<sensor> gone` only when it had an open problem | FR-019 |
| http | `http <name>: <outcome>[; container <c> …]`, e.g. `PROBLEM: http web-app: no answer within 15s`; the entry name leads, like command and freshness texts | — |
| health | `CHECKER ERROR: health: <n> checks did not finish before the tick deadline (<budget>), the host may be overloaded: <check>, …` (key `health/tick/overrun`) when two or more checks of one tick ran out of the tick deadline; a single one keeps `CHECKER ERROR: <check>: run did not finish/not started before the tick deadline (<budget>)` | — |
| delivery | prefix on the next delivered Telegram message: `Undelivered earlier:`, then one `PROBLEM: <text> (since …), no longer open` line per listed PROBLEM that fits, then `… and N more undelivered PROBLEMs` when more were listed than the list holds (that line is always shown); lines that do not fit wait for the next delivered message, and a PROBLEM leaves the list only once a delivered message has shown it | FR-031 |

Checker errors added (key ⇒ text):

| Key | Text |
|---|---|
| `disks/<serial>/no-verdict` | read returned no health verdict; health not judged |
| `disks/<serial>/<section>` (`attributes`, `nvme-health-log`, `selftest-log`) | read lacks <section>; previous state kept |
| `disks/<serial>/no-temperature` | read returned no temperature |
| `disks/<serial>/forced-read` | drive asleep since <time> could not be read when woken: <error> |
| `disks/discovery/empty` | no drives found while <n> are known; missing-drive check suspended |
| `temps/cpu/none` | no CPU package sensor found |
| `temps/.../unreadable`, `temps/gpu/<n>/unreadable` | sensor present but unreadable; state kept |
| `kernel/kmsg/lost` | <n> kernel records were lost before they could be read |
| `kernel/boottail/incomplete` | previous boot's kernel log tail was not read completely: <time limit \| record cap> |
| `kernel/<mc>/edac-unreadable` | memory controller <mc> counters unreadable: <error> |
| `health/maintenance/stuck` | maintenance lock held since <time> (more than <max>) |
| `notify/telegram/failing` | Telegram delivery failing since <time> (sent by email only, once per failure) |
| `http/<name>/auth` | endpoint refused the credentials (<status>) |
| `freshness/<name>/unreadable` | stamp endpoint did not answer / answered <status> / body is not a Unix time |

## SMS fallback (FR-030)

While Telegram delivery is failing, each queued PROBLEM is also sent by SMS (plain text, same
host prefix); the RESOLVED of a PROBLEM that went by SMS this way is also sent by SMS. Events and
other RESOLVED messages wait for Telegram. Every message sent by SMS this way is still delivered
to Telegram once it recovers.
