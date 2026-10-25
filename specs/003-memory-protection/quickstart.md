# Quickstart: validating Memory Protection

Every scenario below is a unit test against fixtures and fakes unless marked **host**. Run all
with `task test` (or `task test:coverage` for the gate). Messages must match
[contracts/messages.md](contracts/messages.md); configuration is
[contracts/config.md](contracts/config.md).

## Guard (`pkg/health/memguard`, fake clock, fake actuator, fake reporter)

| Spec | Feed | Expect |
|---|---|---|
| US1-1, SC-001 | 64 GB host; `web-app` anon +1 GB/min; available falls below 5 %, `full avg10` 35 % | restart of `web-app` ≤ 60 s after both hold; one `restarted` outcome on `mode` |
| US1-2 | `db` 40 GB flat, `worker` 2→9 GB | `worker` chosen |
| US1-3 | one 1 s reading in danger, then normal | no action |
| US1-4, SC-005 | second danger 2 h after a restart of the same service | `stop`, outcome on `failed_mode` |
| US1-5, SC-004 | culprit in `never_touch` | no action; one `protected` outcome |
| US1-6 | danger persists 2 min after restart | one `not_recovered`; no second action in the episode |
| US1-7 | `intervene: report` | no actuator call; `would_restart` outcome |
| US1-9 | culprit is `unit:backup.service` | `systemctl restart` via fake; outcome names the unit |
| edge | actuator blocks past `action_timeout` | `cgroup.kill` via fake; `killed` on `failed_mode` |
| edge | no growing service | one `no_culprit` with the top process |
| edge | restart of tgshd mid-episode (export → restore) | repeat rule still applies |
| edge | `intervene: off` | guard not started |

## Memory check (`pkg/health/check/memory`)

| Spec | Feed | Expect |
|---|---|---|
| US2-1 | available 9 % for 5 min | `memory/available` PROBLEM text with top consumer |
| US2-2, SC-003 | `web-app` +20 GB/h on 64 GB host | growth PROBLEM ≥ 30 min before 5 % available |
| US2-3 | back to normal | RESOLVED |
| US2-5, SC-006 | sawtooth cache 2→8→2 GB hourly; 40 GB flat db | no PROBLEM |
| US3-1/2/3/4 | containers with / without limit / excluded / restart policy `no` | one `memory/limit` PROBLEM naming the counted ones largest first; RESOLVED when none is left |
| US3-5 | a container joins the open set; tgshd restarts | one event for the joiner, not repeated after the restart; none before `threshold` |
| edge | a list too long for one message | the last names cut to `… and K more` |
| edge | `SwapTotal 0` | no swap condition |
| edge | PSI file missing | checker error once; other judgements continue |

## Kernel (`pkg/health/check/kernel`, synthetic kmsg and journal lines)

| Spec | Feed | Expect |
|---|---|---|
| US4-1, SC-007 | `oom-kill:constraint=CONSTRAINT_NONE,…,task_memcg=/system.slice/docker-<id>.scope,task=python3,pid=4242` + `Out of memory: Killed process 4242 (python3) … anon-rss:56623104kB` | `Kernel: out of memory, killed python3 (pid 4242) in container web-app, 54 GB` |
| US4-2 | same lines in the previous-boot journal | `Kernel (previous boot): …` once |
| US4-3 | `constraint=CONSTRAINT_MEMCG` | "hit its memory limit" wording |

## Reports, config, wiring

- `/status` memory block renders as in the contract (`pkg/health/report`).
- Config: every key's default, range and cross-check (`pkg/health/config`), including
  `danger_available_percent ≥ warn_available_percent` rejected.
- `check --once` prints the memory readings and the guard mode (`cmd`).

## **Host** validation (manual, on a test host — never on a production host)

1. Configure `memory.guard.intervene: report`, `danger_for: 10s`, a tiny `culprit_min_growth`.
2. Run a throwaway container with `--restart unless-stopped` that allocates memory steadily
   (e.g. a loop appending to a buffer) without a memory limit; watch the `would restart` report
   and the limit-audit PROBLEM (a container with any other restart policy, `no` or `on-failure`, is not audited).
3. Switch to `act`; repeat; the container is restarted within 60 s of danger and one Telegram
   message arrives. Repeat once more inside 6 h: it is stopped and an SMS arrives.
4. While the host is in danger, confirm `grep VmLck /proc/$(pidof tgshd)/status` is non-zero and
   `cat /proc/$(pidof tgshd)/oom_score_adj` is `-1000`.

## Validation record (T020, 2026-10-25)

Automated gates, run on `main` at the end of the feature:

| Gate | Command | Result |
|---|---|---|
| Lint | `task lint` | 0 issues |
| Tests and coverage | `task test:coverage` | all packages pass under the race detector; measured total over the gate scope 97.3 % (floor 80 %) |
| Static build | `CGO_ENABLED=0 task build` | succeeds, `bin/tgshd` produced |
| Private-data scan | `git diff e9d10e3..HEAD` (excluding `.beads/` and `go.sum`) searched for IPv4 addresses, the operator's private host and service names (list kept outside the repo), personal names and email addresses | no hits; the only match of the author handle is the public module path `github.com/Toshik1978/tgshd` |

Host validation (SC-001, SC-002): **pending, left to the operator.** It must be run by hand on a
test host (never production) following the steps above; it was deliberately not run during
implementation because it drives real memory pressure.
