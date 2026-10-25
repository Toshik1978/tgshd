# Feature Specification: Memory Protection

**Feature Branch**: `003-memory-protection`

**Created**: 2026-10-25

**Status**: Draft

**Input**: User description: "Memory protection for tgshd: proactive alerting and automatic intervention when a service's memory runs away, especially at night when the operator is asleep."

Adds memory monitoring to host health monitoring (001, 002): early warnings when memory runs
short or a container keeps growing, a warning about containers that run without a memory limit,
clear reports of out-of-memory kills, and — the heart of the feature — an automatic intervention
that restarts (or, for a repeat offender, stops) the container whose memory is running away
before the host starves.

This feature builds on [001 Host Health Monitoring](../001-host-health-monitoring/spec.md) and
[002 Deeper Health Checks](../002-deeper-health-checks/spec.md). Everything there that is not
amended here stays in force.

**Why.** One night a containerised service on the monitored host leaked tens of gigabytes of
memory within about an hour. The host started thrashing, the kernel killed the service twice, and
tgshd's own checks ran out of time, so the only alerts that went out were unclear side effects.
The operator was asleep and nothing stopped the leak. That service has since been fixed, but the
next leak can come from any service, so the protection must not depend on knowing the culprit in
advance, and it must keep working while the host is starving.

## Clarifications

### Session 2026-10-25

- Q: Should the automatic intervention also restart leaking host services that run outside Docker (systemd units), or only containers? → A: Containers and systemd services, both restarted by the same rules; stray processes outside any service are only reported.
- Q: When the memory section is first added, should tgshd begin in report-only mode until switched to act? → A: Yes. One intervention setting with three values — `off`, `report`, `act` — defaulting to `report` when the memory section is present.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A runaway service is restarted before the host starves (Priority: P1)

While the operator sleeps, a service — a container or a systemd service — starts leaking memory. Once the host is in real danger —
memory pressure stays high and little memory is left, not just one bad reading — tgshd picks the
service whose memory grew the most recently, restarts it, checks that memory came back, and
tells the operator what it did. If the same service runs away again soon after its restart,
tgshd stops it instead of restarting it again, and wakes the operator. Services on the
operator's never-touch list are never restarted or stopped.

**Why this priority**: This is the outcome the operator asked for: the night of the leak would
have ended with one restart and a Telegram message instead of a thrashing host. It must work
precisely when the host is starving, which is when the regular checks fail.

**Independent Test**: Feed recorded memory and pressure readings and container memory series
(a steady leak in one container; a big but stable container; a leak in a never-touch container;
a second leak after a restart; a restart that frees nothing) to the intervention logic with a fake
container controller and a fake notifier, and verify which container is acted on, which action is
taken, and which message goes out on which channel. Separately, verify on a test host that the
intervention still acts while the host is under heavy memory pressure.

**Acceptance Scenarios**:

1. **Given** intervention is enabled and one container has grown by several gigabytes in the last minutes, **When** memory pressure stays above the danger level and available memory stays below the danger floor for the configured duration, **Then** that container is restarted and one Telegram message names it, its memory before the restart and the host's available memory.
2. **Given** the same danger, **When** the largest container has been stable and a smaller container is the one growing, **Then** the growing container is restarted and the largest is left alone.
3. **Given** a single reading above the danger level, **When** the next reading is back to normal, **Then** nothing is restarted.
4. **Given** a container was restarted by tgshd, **When** it again becomes the culprit within the repeat window, **Then** it is stopped, not restarted, and the message goes out by SMS.
5. **Given** the culprit is on the never-touch list, **When** the danger persists, **Then** nothing is restarted or stopped, and one SMS names the culprit and says that tgshd did not act because the container is protected.
6. **Given** a container was restarted, **When** the danger has not cleared within the recovery window after the restart, **Then** one SMS says the restart did not free memory, and no second container is restarted in the same episode.
7. **Given** intervention is in `report` mode (the default), **When** the danger persists, **Then** nothing is restarted or stopped, and the message says which container would have been restarted and why.
8. **Given** the host is starving so badly that the regular checks miss their deadlines, **When** the danger conditions hold, **Then** the intervention still acts within its time target (SC-001).
9. **Given** the growing service is a systemd service on the host, not a container, **When** the danger conditions hold, **Then** that service is restarted by the same rules and the message names its unit.

---

### User Story 2 - Early warning before memory runs out (Priority: P2)

The operator is warned while there is still time to act: when memory pressure rises, swap fills,
available memory runs low, or one container keeps growing for a long time. Each warning names the
biggest consumer and its growth, so the operator knows at a glance what is wrong. Warnings resolve
like every other problem, and `/status` and `/history` show them.

**Why this priority**: Most memory trouble builds up over hours or days. A warning lets the
operator fix it calmly in the daytime, before the intervention ever has to act.

**Independent Test**: Feed recorded readings (normal; available memory below the warning floor;
swap above the warning level; high pressure; one container growing steadily for an hour) and
verify the PROBLEM and RESOLVED texts, the named consumer and the growth figures.

**Acceptance Scenarios**:

1. **Given** available memory stays below the warning floor for the threshold, **When** the check runs, **Then** one PROBLEM states the available memory and names the biggest consumer, e.g. `memory: 9% available (5.6 of 62 GB); top: web-app 31 GB`.
2. **Given** a container's memory grows steadily for the growth window, **When** the check runs, **Then** one PROBLEM names it and its growth, e.g. `memory: web-app grew 12→31 GB in 40 min`, even while the host still has plenty of memory left.
3. **Given** an open memory problem, **When** the readings return to normal, **Then** one RESOLVED is sent.
4. **Given** memory problems are open, **When** the operator sends `/status`, **Then** the current available memory, swap use, pressure and the top consumers are shown.
5. **Given** a container that grows and shrinks (a cache that is flushed regularly), **When** its peak is within its normal range, **Then** no growth PROBLEM opens.

---

### User Story 3 - Containers without a memory limit are pointed out (Priority: P3)

A memory limit on a container is the safety net that works even when tgshd cannot run: the
kernel then kills only that container instead of starving the host. tgshd warns about containers
that run without one, so the operator can add limits where they matter.

**Why this priority**: It closes the gap that tgshd itself cannot close, at almost no cost, but
it does not act by itself.

**Independent Test**: Feed container lists with and without limits, and with an exclusion, and
verify which warnings open and resolve.

**Acceptance Scenarios**:

1. **Given** a running container without a memory limit that is not excluded from the audit, **When** the check runs, **Then** one problem names it.
2. **Given** that problem is open, **When** the container is recreated with a limit, **Then** the problem resolves.
3. **Given** a container the operator excluded from the audit, **When** it runs without a limit, **Then** no problem opens.

---

### User Story 4 - Out-of-memory kills are reported clearly (Priority: P3)

When the kernel kills a process because memory ran out — during this boot or the previous one —
tgshd reports it in plain words: which process, which container it belonged to, and how much
memory it held.

**Why this priority**: A kill by the kernel is the clearest sign that protection was needed; today
it is reported only indirectly, through its side effects.

**Independent Test**: Feed recorded kernel log lines of out-of-memory kills (host-wide and inside
a container's limit, current and previous boot) and verify the texts.

**Acceptance Scenarios**:

1. **Given** the kernel kills a process inside a container, **When** the kernel check runs, **Then** one alert names the process, its container and the memory it held, e.g. `Kernel: out of memory, killed python3 (pid 4242) in container web-app, 54 GB`.
2. **Given** the kills happened during the previous boot, **When** the host has rebooted, **Then** they are reported once, marked as from the previous boot.
3. **Given** a kill caused by a container reaching its own memory limit, **When** it is reported, **Then** the text says the container hit its limit, not that the host ran out of memory.

---

### Edge Cases

- Several services grow at once: the one that grew the most in the recent window is the culprit, whether it is a container or a systemd service; if growth is small everywhere and memory is eaten by a process outside any service, nothing is restarted and the message names that process instead.
- A container's processes also belong to the container runtime's systemd services on the host: a container is always judged as the container, never as the runtime's service.
- The culprit restarts but its memory comes straight back (a leak that starts at boot): this counts as a repeat within the repeat window, so it is stopped.
- The container runtime or systemd does not answer during danger (a restart hangs or cannot be sent): after the bounded time (FR-010) the culprit's processes are killed directly through the kernel, which frees its memory whatever the runtime's state; whether it comes back is left to its restart policy, and one SMS says what happened.
- tgshd restarts while an intervention episode is in progress: the restart history (who was restarted when) survives, so a repeat leaker is still stopped rather than restarted again.
- A maintenance window is open: intervention still acts (a starving host is not maintenance), but warnings follow the usual quiet-window rules.
- The intervention itself must not be what starves: it keeps working when the host is out of memory and swap, and does not depend on the state store, the network or Telegram to make its decision.
- The host has no swap: swap warnings never open.
- A container is removed or renamed between readings: its growth history is dropped and it cannot be a culprit until it has new history.
- The memory section is absent from the configuration: no memory check runs and nothing is ever restarted.

## Requirements *(mandatory)*

### Functional Requirements

#### Intervention

- **FR-001**: tgshd MUST provide a memory intervention with one setting of three values: `off` (no intervention), `report` (it reports what it would do and does nothing else) and `act`. In `report` mode a "would restart" or "would stop" report goes on the channel of a fixed problem (FR-007, Telegram by default), so a preview never wakes the operator; a protected culprit or a culprit-less danger is reported as in `act` mode, on the channel of an unfixed problem, because nobody fixes it. When the memory section is present without the setting, it MUST be `report`. *Why: an automatic restart must be the operator's explicit choice, and a report-only start lets them check the thresholds pick the right service before anything is restarted.*
- **FR-002**: The intervention MUST act only when the host is in danger: memory pressure above a configured danger level AND available memory below a configured danger floor, both continuously for a configured duration. *Why: a single bad reading, or high pressure with plenty of memory left, must never restart a service.*
- **FR-003**: The intervention MUST choose as culprit the eligible service — a container or a systemd service on the host — whose memory grew the most over a configured recent window, and MUST NOT choose a service whose memory did not grow in that window, however large it is. Containers and systemd services are judged and acted on by the same rules (FR-003 to FR-010). *Why: the leaking service is the one growing; big but stable services are healthy; a leak in a service that runs directly on the host is as dangerous as one in a container.*
- **FR-004**: Every container and systemd service MUST be eligible except those on a configured never-touch list, and except tgshd itself and the services the host cannot run without (the container runtime, the system's own core services), which are never eligible. *Why: tomorrow's culprit is unknown, but some services (e.g. a database) must never be restarted by a machine, and restarting the host's core services would do more harm than the leak.*
- **FR-005**: The intervention MUST restart the culprit, unless the same container was restarted by tgshd within a configured repeat window, in which case it MUST stop it. *Why: a restart clears a leak and keeps the service; restarting a repeat leaker again would be a loop.*
- **FR-006**: After acting, the intervention MUST NOT act again within the same danger episode until a configured recovery window has passed; if the danger has not cleared by then, it MUST report that the action did not free memory and MUST NOT restart a second container in that episode. *Why: guessing again while the host is starving risks restarting healthy services.*
- **FR-007**: Every intervention outcome MUST be reported: a successful restart by Telegram; a stop of a repeat leaker, a restart that did not free memory, a protected culprit, a failed or impossible action, by SMS. Each report MUST name the service, the action, its memory before the action and the host's available memory. The channels MUST be overridable with the existing per-check delivery mode. *Why: when tgshd fixed the problem, the operator need not wake up; when it could not, they must.*
- **FR-008**: The intervention MUST keep making decisions and acting while the host is starving: independent of the regular check schedule and its deadlines, not slowed by the state store, the network or Telegram, and protected from being killed or swapped out for lack of memory. Reports MAY be delivered later than the action. *Why: the regular checks failed on the night of the leak; the intervention exists for exactly that situation.*
- **FR-009**: The restart history used by FR-005 MUST survive a tgshd restart. *Why: otherwise a crash or deploy during an episode would reset the loop protection.*
- **FR-010**: Each action against the container runtime MUST be bounded in time; a timeout counts as a failed action. *Why: a hung restart must not hang the intervention.*
- **FR-011**: If the memory is held by a process that belongs to no container and no systemd service, the intervention MUST NOT act and MUST report that process (name, pid, memory) by SMS instead. *Why: a stray process has nothing to restart it, so killing it is out of scope; the operator must still learn what is eating memory.*

#### Early warning

- **FR-012**: A `memory` check MUST raise a PROBLEM when available memory stays below a configured warning floor, when swap use stays above a configured warning level (only on hosts with swap), or when memory pressure stays above a configured warning level, each for the check's threshold. *Why: these are the signs of trouble building up while there is still time.*
- **FR-013**: The `memory` check MUST raise a PROBLEM when one container's memory grows by more than a configured amount over a configured growth window, naming the container and its growth from start to end of the window. *Why: a slow leak is visible long before the host runs short.*
- **FR-014**: Every memory PROBLEM text MUST name the current top consumer (container or host process) and its memory. *Why: the operator must know at a glance what is wrong.*
- **FR-015**: Memory problems MUST resolve, be rate limited, honour quiet windows and appear in `/status`, `/history` and the weekly summary like every other check's problems. *Why: consistency with the existing alerting.*
- **FR-016**: `/status` MUST show the current available memory, swap use, memory pressure and the top consumers. *Why: the operator can check on demand.*
- **FR-017**: Memory and per-container samples MUST be stored so that growth over the configured windows can be judged after a tgshd restart. *Why: a leak that started before a restart must still be recognised.*

#### Audit and kernel kills

- **FR-018**: The `memory` check MUST raise a problem for each running container that has no memory limit, except containers excluded from the audit; it resolves when the container runs with a limit or stops. *Why: a limit is the safety net that works even when tgshd cannot.*
- **FR-019**: The kernel check MUST report out-of-memory kills from the current and the previous boot, naming the killed process, its container (when it belonged to one), the memory it held, and whether the kill came from a container reaching its own limit or from the host running out. *Why: kernel kills are today reported only through their side effects.* (Extends 002's kernel classes.)

#### Configuration

- **FR-020**: All thresholds, windows, the never-touch list, the audit exclusions, report-only mode and the delivery mode MUST be configurable in the health configuration file, with documented defaults; the memory check MUST run only when its section is present, and an invalid memory section MUST be reported as a checker error without stopping the daemon. *Why: consistency with the existing configuration rules.*

### Key Entities

- **Memory snapshot**: the host's total and available memory, swap total and used, and memory pressure, at one moment.
- **Service**: a container or a systemd service on the host — the unit the intervention judges and acts on.
- **Service memory sample**: one service's memory use at one moment; a series of them gives its growth over a window.
- **Danger episode**: the period during which the danger conditions hold; it has a start, the culprit chosen, the action taken and its outcome.
- **Intervention record**: a service acted on, when, which action, its memory before, and whether memory recovered; kept across tgshd restarts for the repeat window.
- **Never-touch list / audit exclusions**: container and systemd service names the operator configured (the audit covers containers only).

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In a replay of a leak like the one that triggered this feature (a container growing by about 1 GB per minute on a host with 64 GB), the leaking container is restarted within 60 seconds of the danger conditions being met, and before the host's available memory reaches zero.
- **SC-002**: In the same replay, the operator receives exactly one message for the intervention, by Telegram, and no SMS.
- **SC-003**: A steady leak of 20 GB per hour is warned about at least 30 minutes before the danger conditions would be reached.
- **SC-004**: A container on the never-touch list is never restarted or stopped in any test scenario.
- **SC-005**: No container is restarted more than once within the repeat window in any test scenario.
- **SC-006**: Big but stable containers and regularly flushed caches cause no warnings and no interventions in a week-long replay of normal host behaviour.
- **SC-007**: Every out-of-memory kill in the test kernel logs is reported with the process name and, when it has one, its container.

## Assumptions

- Intervention acts on containers and systemd services. Processes outside both are named in reports but never killed.
- The container runtime is the one the containers check already uses; containers are identified by name, systemd services by unit name, and the never-touch list may name either.
- A report in `report` mode is phrased as what would have happened (`would restart …`) so it can never be mistaken for an action.
- Default thresholds (adjustable; the plan finalises them): warning floor 15% available, swap warning 80%, danger floor 5% available, danger pressure sustained for 30 s, growth window 1 h for warnings and 10 min for culprit choice, repeat window 6 h, recovery window 2 min.
- Restarting a container resets its memory; services that need a graceful stop get the container runtime's normal stop timeout, bounded by FR-010.
- The intervention runs inside the tgshd daemon on the monitored host; no other host or service is involved.
- Memory limits are recommended but not enforced: the audit only warns.
