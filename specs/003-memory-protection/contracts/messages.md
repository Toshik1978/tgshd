# Contract: Memory messages

Amends [002 contracts/telegram-commands.md](../../002-deeper-health-checks/contracts/telegram-commands.md)
("Pushed messages", `/status`). Sizes are shown in GB with one decimal below 10 GB and none above
(`5.6 GB`, `31 GB`); `<top>` is `<name> <size>` of the largest service, or of the largest process
(`python3 (pid 4242)`) when no service holds most of the memory. Every warning but the limit list
ends with `; top: <top>`: that list is already sorted by size.

## Warnings (PROBLEM / RESOLVED, channel `mode`)

```text
PROBLEM: memory: 9% available (5.6 of 62 GB); top: web-app 31 GB (since 01:10)
PROBLEM: memory: swap 85% used (6.8 of 8 GB); top: web-app 31 GB
PROBLEM: memory: pressure 12% (stalled, 1 min avg); top: web-app 31 GB
PROBLEM: memory: web-app grew 12→31 GB in 40 min; top: web-app 31 GB
PROBLEM: memory: 3 containers run without a memory limit: web-app 31 GB, worker 3 GB, cache 1.2 GB
RESOLVED: memory: 45% available
RESOLVED: memory: swap 20% used
RESOLVED: memory: pressure 0%
RESOLVED: memory: web-app at 13 GB
RESOLVED: memory: every container runs with a memory limit
```

The limit audit is one condition, key `memory/limit`. It names every counted container, largest
first; only when the text would not fit one message are the last names cut to `… and K more`. A
container that joins the set while the problem is open is announced once, as an event on the
check's `mode`: `memory: container queue-ok runs without a memory limit`. A container leaving
the set changes the open problem's text only.

A percentage is whole, except where rounding would land on the warn level itself: a problem reads
`14.6% available` (floor 15), never `15% available`. A span under a minute reads `1 min`.

## Guard outcomes (events, key `memory/guard/<service>`)

| Outcome | Channel | Text |
|---|---|---|
| restarted | `mode` | `memory: restarted container web-app (grew 12→31 GB in 10 min); host 4% available, stalled 35%` |
| would restart (report mode) | `mode` | `memory: would restart container web-app (grew 12→31 GB in 10 min); host 4% available, stalled 35% — intervene is report` |
| stopped (repeat) | `failed_mode` | `memory: stopped container web-app: ran away again 2h10m after tgshd restarted it (grew 3→22 GB in 10 min); host 4% available` |
| killed (runtime did not answer) | `failed_mode` | `memory: killed service backup.service through the kernel: systemctl did not answer within 60s (grew …); host 2% available` |
| killed (runtime unreachable) | `failed_mode` | `memory: killed service backup.service through the kernel: systemctl could not be reached (grew …); host 2% available` (`docker could not be reached` for a container) |
| not recovered | `failed_mode` | `memory: restarting web-app (31 GB before) did not free memory; host still 3% available, stalled 45%; top: worker 20 GB` |
| protected | `failed_mode` | `memory: host 3% available, stalled 45%; db is growing (12→40 GB in 10 min) but is never-touch, tgshd did not act` |
| no culprit | `failed_mode` | `memory: host 3% available, stalled 45%; no service is growing; top process: python3 (pid 4242) 40 GB in user.slice` |
| action failed | `failed_mode` | `memory: could not restart web-app (31 GB): <error>; host 3% available` |

"stalled" is `full avg10` pressure: the share of time every task waited for memory.

## Kernel (events, key `kernel/oom`, channel of the kernel check)

```text
Kernel: out of memory, killed python3 (pid 4242) in container web-app, 54 GB
Kernel: container web-app hit its memory limit, killed python3 (pid 4242), 7.9 GB
Kernel: out of memory, killed worker (pid 977) in backup.service, 3.1 GB
Kernel (previous boot): out of memory, killed python3 (pid 4242) in container web-app, 54 GB
Kernel: … and 3 more out-of-memory kills
```

## `/status`

A memory block after the open problems:

```text
Memory: 45% available (28 of 62 GB), swap 20% used, stalled 0%
Top: web-app 13 GB, db 9.1 GB, worker 2.4 GB
Guard: report — last: would restart web-app 2 Oct 01:12
```

When the problem list is too long for one message it keeps the first problems
that fit, drops the later ones and ends with a line `… and N more` (N is the
number of problems left out). The fixed lines around the list (the head lines
before it and the memory block after it) are always kept.

A reading close to its warn level shows one decimal, cut on the problem side,
so it never prints as the level itself (14.96% at a floor of 15 reads `14.9%`).
