# Feature Specification: Host Health Monitoring

**Feature Branch**: `001-host-health-monitoring`

**Created**: 2026-10-16

**Status**: Draft

**Input**: User description: "Host health monitoring in tgshd: replace a cron-driven set of shell health checkers with long-running checks inside the tgshd daemon, with persisted state, history, and two-way Telegram."

## Overview

Operators of a self-hosted Linux server today often watch its health with a set
of shell scripts run from cron: they read disk health, temperatures, kernel
errors, containers, web endpoints, the UPS, backups and the network, and post
problems to Telegram or SMS. Those scripts work, but most of their size is
plumbing that exists only because each run starts from nothing: state files,
locks, streak bookkeeping, heartbeat files and a degraded mode for a read-only
filesystem. That makes them expensive to change safely.

This feature moves host health monitoring into the tgshd daemon. The daemon
keeps state between checks, persists it and a history of alerts and readings,
and answers questions about the host over Telegram. The primary goal is
maintainability: the same alerting behaviour operators rely on today, with far
less code that is unit-tested instead of shell-tested.

tgshd is a public open-source project. The software knows **kinds** of checks;
which containers, endpoints, files, phone numbers and thresholds apply to a
given host is operator configuration that never lives in the project.

## Clarifications

### Session 2026-10-16

- Q: Who may use tgshd's Telegram commands (existing and new)? → A: The configured chat plus an optional list of extra allowed Telegram user/chat IDs from the environment; updates from anywhere else are ignored silently and logged.
- Q: How does the operator retire a removed drive or clear a stuck problem? → A: Telegram commands only (`/retire <drive>`, `/clear <condition>`), from the allowed chats; no local control channel.
- Q: Where do checker errors (missing tool, unreadable source, invalid config) go, now that cron mail no longer carries them? → A: Telegram, labelled as checker errors, and email when an SMTP relay is configured; each distinct error at most once per 24 h on both; always written to the log.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Trustworthy alerts that survive restarts (Priority: P1)

The operator enables health monitoring and configures the basic checks (the
network and the UPS). tgshd evaluates them on a schedule and sends one
PROBLEM message when a condition has been broken for its whole threshold, and
one RESOLVED message when it recovers. Short blips that heal on their own stay
silent. When the gateway or the internet is down, the alert goes out by SMS,
because Telegram cannot be reached. The state of every condition survives a
daemon restart or a reboot, so nothing is announced twice and nothing is lost.
The operator can ask `/status` at any time, an external watchdog can see the
daemon is alive through a heartbeat file, and a one-shot mode lets the
operator verify every configured check on a live machine without sending or
storing anything.

**Why this priority**: Every other story relies on this alert engine, its
persistence and its quiet-window rules. On its own it already replaces the
most critical alert today: "the network or the UPS is gone".

**Independent Test**: Configure only the network and UPS checks, simulate a
gateway outage longer than the threshold, a recovery, a daemon restart in the
middle of an outage, and a maintenance window; verify exactly one SMS PROBLEM,
one RESOLVED, no duplicate after restart, nothing during maintenance, and a
fresh heartbeat throughout.

**Acceptance Scenarios**:

1. **Given** a check with a 5-minute threshold, **When** it fails for 3 minutes and then passes, **Then** no message is sent.
2. **Given** a check with a 5-minute threshold, **When** it fails continuously for 5 minutes, **Then** exactly one PROBLEM message is sent stating since when the condition has been broken, and no further message is sent while it stays broken.
3. **Given** an open problem, **When** the first good reading arrives, **Then** exactly one RESOLVED message is sent.
4. **Given** an open problem, **When** the daemon restarts and the condition is still broken, **Then** no new PROBLEM is sent; **When** it later recovers, **Then** one RESOLVED is sent.
5. **Given** the default gateway does not answer, **When** the network threshold passes, **Then** the PROBLEM goes by SMS, checks that need the internet are not judged, and only that one alert is sent for the outage.
6. **Given** the maintenance lock is held exclusively by another process, **When** a cycle runs, **Then** no condition starts, advances or clears, the heartbeat is still written, and the lock is never held by tgshd.
7. **Given** the host booted less than the boot grace period ago, **When** a cycle runs, **Then** nothing is judged and the heartbeat is still written.
8. **Given** a reading cannot be taken (a required tool is missing or a source does not answer), **When** a cycle runs, **Then** the condition is neither started, advanced nor cleared, and the checker error is logged, rate-limited. (Exception: a UPS service that hangs counts as broken, per FR-032.)
9. **Given** the operator sends `/status`, **When** problems are open, **Then** each open problem is listed with its text and since when; **When** none are open, **Then** the reply says all checks are fine.
10. **Given** the operator runs the one-shot mode, **When** it finishes, **Then** every configured check has printed its reading and verdict, and no message was sent and no state was changed.
11. **Given** a reboot script announces a planned reboot of N seconds, **When** the heartbeat is next read, **Then** it carries the end of the planned window; announcing 0 clears it.

---

### User Story 2 - Hardware health without configuration (Priority: P2)

With hardware checks enabled, tgshd discovers the host's drives, temperature
sensors and GPU by itself and alerts when a drive reports failing health, a
critical warning, low spare capacity or high wear, when an error counter
rises, when a self-test is stale or failed, when a drive disappears, when a
temperature exceeds its threshold, or when the kernel reports hardware or
memory errors. Swapping a drive needs no configuration change.

**Why this priority**: Hardware fails quietly and is the part of the old
checks with the most logic; it is the largest maintainability gain.

**Independent Test**: Feed recorded drive-health, sensor and kernel-log
readings representing healthy, degrading and failing hardware; verify the
alerts, the per-drive baselines keyed by serial, and the drive
missing/retired messages.

**Acceptance Scenarios**:

1. **Given** an empty state, **When** the first hardware cycle runs, **Then** one message announces every discovered drive.
2. **Given** a drive's error counter was 0 at its baseline, **When** it reads 2, **Then** one alert names the drive and the rise; **When** it later reads 3, **Then** another single alert is sent for that rise.
3. **Given** a drive that reports no value for a given error attribute, **When** a cycle runs, **Then** that attribute is treated as not reported, not as an error.
4. **Given** a known drive is no longer present, **When** a cycle runs, **Then** a "drive missing" problem opens; **When** the operator retires that drive, **Then** all its problems clear with a single "drive retired" message.
5. **Given** a removable, empty or virtual block device, **When** discovery runs, **Then** it is skipped.
6. **Given** kernel hardware errors appear, **When** they first appear, **Then** one message is sent; **While** they continue, **Then** at most one "still happening" message per 6 hours; **After** a day without them, **Then** the condition goes quiet.
7. **Given** two NVMe drives with identical sensor names, **When** one is overheating, **Then** the alert names the right drive.
8. **Given** no NVIDIA GPU or no memory-error reporting on the host, **When** a cycle runs, **Then** those readings are simply absent and no error is raised.

---

### User Story 3 - Service health from operator configuration (Priority: P3)

The operator lists, in a configuration file, the web endpoints, freshness
files (for example the success stamps backup jobs write) and probe commands
that matter on this host. tgshd also discovers the containers that are meant
to stay running. It alerts when a container is down, unhealthy or crash
looping, when an endpoint stops answering, when a stamp gets too old, or when
a probe fails, and it can optionally run a fix action after repeated failures.

**Why this priority**: Covers the second big group of old checks; depends
only on the alert engine.

**Independent Test**: With fake container, HTTP, file and command sources,
simulate a container crash loop, an endpoint outage, a stale stamp and a probe
that fails twice with a fix action configured; verify alerts, the
dependency-skip rule, and the fix action's rate limit.

**Acceptance Scenarios**:

1. **Given** a container whose restart policy keeps it running, **When** it restarts repeatedly but is "running" at most samples, **Then** it is still treated as broken and alerts after its threshold.
2. **Given** a container that the operator started by hand without a restart policy, **When** it stops, **Then** nothing is reported.
3. **Given** an endpoint check that depends on another check, **When** the other check is failing, **Then** the dependent check is not judged (no double paging).
4. **Given** a freshness file that has never existed, **When** checks run, **Then** a problem opens only after the maximum age has elapsed since the file was first seen missing.
5. **Given** a probe with a fix action after 2 strikes and at most 3 fixes per hour, **When** it fails twice in a row, **Then** the fix runs once and is recorded; **When** it would be the fourth fix within an hour, **Then** it does not run.
6. **Given** a container event that carries a health-check command line, **When** it is reported or logged, **Then** only the container name, action and state appear.
7. **Given** the container runtime is unavailable at start-up, **When** the daemon starts, **Then** all other checks run and container readings count as "cannot read".

---

### User Story 4 - Incoming SMS and a proven modem (Priority: P4)

tgshd takes over incoming SMS from the GSM modem's SMS gateway: every received
message is forwarded to Telegram the way messages from the other supported
modem already are. Once a week tgshd sends a test SMS to the modem's own
number and checks it comes back, so a dead modem or SIM is found before it is
needed in an outage.

**Why this priority**: The SMS fallback is only as good as the modem; this
proves it. It depends on the SMS gateway being configured.

**Independent Test**: With a fake SMS gateway store, inject incoming messages
(single and multi-part) and the probe reply; verify forwarding, processing
marks, the probe match, and the "did not come back" alerts.

**Acceptance Scenarios**:

1. **Given** an unprocessed incoming SMS, **When** tgshd reads it, **Then** it is forwarded to Telegram once, multi-part texts joined, and marked processed.
2. **Given** the weekly probe was sent, **When** the reply arrives from the modem's own number (sender compared by digits; by the last 9 digits when the two lengths differ) with the exact text, **Then** it is not forwarded and the probe counts as passed.
3. **Given** the probe was sent, **When** 30 minutes pass without the reply, **Then** a PROBLEM is sent; **When** a later probe comes back, **Then** RESOLVED is sent.
4. **Given** no probe has come back for 8 days (for example because probes were skipped), **When** a cycle runs, **Then** a PROBLEM is sent.
5. **Given** the probe send fails, **When** it fails, **Then** a PROBLEM is sent at once.

---

### User Story 5 - History and reports over Telegram (Priority: P5)

The operator can ask what happened and how things are trending: `/history`
lists past alerts (optionally for one check and period), `/temp` and `/disks`
show current readings next to their 7-day minimum, average and maximum, and a
weekly summary message reports each drive, the services, and open problems.

**Why this priority**: New value rather than replacement; it builds on the
data the other stories record.

**Independent Test**: Seed the history with alerts and samples spanning
several months; verify the command replies, retention, roll-up, and the
weekly summary's content and length limit.

**Acceptance Scenarios**:

1. **Given** alerts over the past month, **When** the operator sends `/history <check> 7d`, **Then** only that check's alerts from the last 7 days are listed, newest first.
2. **Given** samples for the last week, **When** the operator sends `/temp`, **Then** each sensor shows its current value and 7-day min/avg/max.
3. **Given** samples older than the raw retention, **When** retention runs, **Then** they are replaced by daily min/avg/max roll-ups that `/temp` and `/disks` still use.
4. **Given** the weekly schedule fires, **When** the summary is built, **Then** it contains per-drive lines (health, wear, data written this week), a services block, and open problems, in one message no longer than the messaging limit, cut with an ellipsis if needed.
5. **Given** a summary section cannot be built, **When** the summary is sent, **Then** that section says "unavailable" and the rest still goes out.

---

### Edge Cases

- The root filesystem goes read-only: the daemon keeps judging from memory, logs once that state cannot be saved, still sends alerts, and saves everything when writing works again.
- Telegram is unreachable while the network is known to be down: failed alerts stay queued and are sent once connectivity returns; no error noise is produced for them.
- A check's configuration is invalid (unknown type, missing field, bad duration): the daemon reports which entry and why, skips that entry, and runs the rest.
- No configuration file is present: the daemon behaves exactly as it does today, with health monitoring off.
- A secret referenced by a check (for example an API token in a header) is missing or still a placeholder: this is a checker error (FR-045), and never counts as the service being down or recovered.
- The SMTP relay is down: the email is logged as not sent; Telegram delivery of the same checker error is unaffected.
- The host clock jumps: streaks count time that had readings; a jump does not open or close problems by itself.
- A problem is stuck and the operator wants it gone: there is a supported way to clear one condition without a RESOLVED message.
- Two daemon instances by mistake: the second refuses to run health monitoring against the same state.
- A device that is removed or swapped (drive, GPU, sensor chip) is reported once and can be retired; nothing about it lingers afterwards.
- A stranger finds the bot and sends it a command in a private chat or from their own group: nothing runs, nothing is replied, the attempt is logged.
- The weekly summary exceeds the message length limit: it is cut with an ellipsis, never split or dropped.

## Requirements *(mandatory)*

### Functional Requirements

**Alert engine**

- **FR-001**: The system MUST evaluate each configured check on its own schedule and judge each condition as good, broken, or unreadable.
- **FR-002**: The system MUST send exactly one PROBLEM per condition once it has been broken continuously for its threshold, stating when the broken streak began, and exactly one RESOLVED on the first good reading after that; nothing in between.
- **FR-003**: An unreadable result MUST NOT start, advance or clear a condition; it MUST be reported as a checker error (FR-045).
- **FR-004**: Thresholds MUST count only time during which readings were taken; downtime, quiet windows and unreadable periods MUST NOT count.
- **FR-005**: The system MUST NOT judge any condition while a configured maintenance lock file is held exclusively by another process, and MUST probe that lock without ever holding it.
- **FR-006**: The system MUST NOT judge any condition during a configurable grace period after host boot (default 10 minutes).
- **FR-007**: Each check MUST have a delivery mode: Telegram, SMS, or Telegram with SMS fallback.
- **FR-008**: Network reachability (gateway, then internet via several targets where any one answering suffices, tolerating a few lost packets) MUST be evaluated first in each cycle and MUST alert by SMS; its RESOLVED MUST go by Telegram, falling back to SMS.
- **FR-009**: While the outside is known to be down, checks marked as needing the internet MUST NOT be judged, and failed Telegram alerts MUST be kept and delivered after connectivity returns.
- **FR-010**: Every pushed health message (PROBLEM, RESOLVED, events, checker errors, the weekly summary) MUST start with the host's name, so several hosts can share one chat; replies to commands are not prefixed.
- **FR-011**: The operator MUST be able to clear a single stuck condition with a Telegram command (`/clear <condition>`) without a RESOLVED message being sent; the command confirms what was cleared, and an unknown condition key is answered with the list of open ones.

- **FR-045**: Checker errors (a reading that cannot be taken, a missing tool, an invalid configuration entry, a secret that is missing) MUST always be written to the log, and MUST be sent to Telegram labelled as a checker error (distinct from PROBLEM/RESOLVED) and by email when an SMTP relay is configured; each distinct error MUST be sent at most once per 24 hours per channel. Email is optional: without SMTP settings only Telegram is used.

**Persistence and history**

- **FR-012**: Condition state (open problems, streak starts, baselines, probe state, queued alerts) MUST survive daemon restarts and host reboots.
- **FR-013**: If state cannot be written, the system MUST continue from in-memory state, report the failure once, and save the current state as soon as writing succeeds again.
- **FR-014**: The system MUST record every PROBLEM, RESOLVED, retirement and fix action in an alert log retained for at least one year.
- **FR-015**: The system MUST record numeric readings (temperatures, drive wear, data written, UPS charge and voltage) per cycle, keep them raw for 90 days, and keep daily minimum/average/maximum beyond that.

**Liveness and operator tools**

- **FR-016**: The system MUST write a heartbeat file each cycle containing the current time and the end of any planned-reboot window, including cycles skipped for maintenance or boot grace.
- **FR-017**: The system MUST offer a command that sets or clears a planned-reboot window (0 to 86 400 seconds) in the heartbeat without running checks.
- **FR-018**: The system MUST offer a one-shot mode that runs every configured check once, prints each reading and verdict, sends nothing and changes no state.
- **FR-019**: Health monitoring MUST start and keep running when the container runtime or the SMS gateway's database is unavailable; only the checks that need them become unreadable.

**Hardware checks**

- **FR-020**: The system MUST discover drives automatically, identify each by serial number, and skip removable, media-less and virtual devices.
- **FR-021**: For each drive the system MUST alert on failing overall health, an NVMe critical warning, available spare at or below its threshold, wear above a configurable level, and any rise of the drive's error counters against its own baseline (one alert per rise). Attributes a drive does not report MUST be treated as not reported.
- **FR-022**: The system MUST alert when a drive's most recent self-test failed or when no self-test has completed within a configurable age.
- **FR-023**: The system MUST alert when a known drive is missing, and MUST let the operator retire a drive with a Telegram command (`/retire <drive>`, by serial) so that all its conditions and its baseline clear with one message.
- **FR-024**: On first run with no stored state, the system MUST announce all discovered drives in one message.
- **FR-025**: The system MUST read CPU, drive, chipset/board and (when present) GPU temperatures, attribute each drive temperature to its drive, and alert against per-class thresholds from configuration.
- **FR-026**: The system MUST detect kernel-reported hardware error classes (storage I/O, NVMe, ATA, machine check, thermal throttling) and memory-controller error counts where the platform reports them; it MUST send one message when they start, at most one "still happening" per 6 hours, and go quiet after 24 hours without them.
- **FR-027**: A CPU, GPU or sensor source missing from an otherwise good reading MUST be reported once as gone and clear its conditions.

**Service checks**

- **FR-028**: The system MUST discover containers whose restart policy keeps them running, minus an operator exclude list, and treat a container as broken when it is not running, restarting, unhealthy, or restarted since the previous observation (crash loop); default threshold 15 minutes.
- **FR-029**: The system MUST NOT include raw container event attributes in any message or log; only container name, action and state.
- **FR-030**: The system MUST support endpoint checks: expected status range, timeout, optional resolution through a DNS-over-HTTPS resolver, optional request header whose value comes from a secret in the environment (never logged), and an optional dependency that skips judging while another check is failing.
- **FR-031**: The system MUST support freshness checks on a file holding a timestamp (or the file's modification time) with a maximum age; a missing file MUST alert only after the maximum age measured from the first cycle that saw it missing.
- **FR-032**: The system MUST check that the UPS monitoring service returns a fresh reading; stale data, a disconnected driver, a failed connection, an empty answer or a hang MUST count as broken.
- **FR-033**: The system MUST support command probes judged by exit status within a timeout, with an optional fix action (restart a container, or run a command) after N consecutive failures, a per-hour fix limit, and a grace period after the target starts.

**SMS**

- **FR-034**: When the SMS gateway is configured, the system MUST read unprocessed incoming messages, forward each to Telegram once (multi-part messages joined), and mark them processed.
- **FR-035**: The system MUST send a weekly test SMS with a random token to the modem's own number and recognise its return (sender matched by digits, by the last 9 digits when lengths differ; text matched exactly) without forwarding it.
- **FR-036**: The system MUST alert when the test SMS cannot be sent, when it has not returned within 30 minutes, or when none has returned for 8 days.

**Telegram commands and reports**

- **FR-037**: The system MUST answer `/status` with open problems (text and since when) or a single all-fine line.
- **FR-038**: The system MUST answer `/history` with the alert log, optionally filtered by check and period, newest first, within the messaging length limit.
- **FR-039**: The system MUST answer `/temp` and `/disks` with current readings and their 7-day minimum, average and maximum.
- **FR-040**: The system MUST send a weekly summary on a configurable schedule (default Monday 09:00): per-drive lines, a services block, and open problems, as one message within the length limit, cut with an ellipsis when too long; a section that cannot be built MUST read "unavailable" without blocking the message.

- **FR-044**: The system MUST accept commands (existing and new) only from the configured chat or from an optional operator-supplied list of extra allowed Telegram user or chat IDs; every other update MUST be ignored without a reply and logged with its sender ID.

**Configuration and openness**

- **FR-041**: Site-specific details (container names, endpoints, file paths, phone numbers, thresholds) MUST come from operator configuration; the project's code, documentation and tests MUST NOT contain any real host's names, addresses, numbers or hardware models.
- **FR-042**: Secrets and on/off switches MUST stay in environment variables as today; check instances MUST come from one configuration file whose path is given in the environment. Without that file, tgshd MUST behave exactly as before this feature.
- **FR-043**: An invalid configuration entry MUST be reported as a checker error with its location and reason, and skipped, without stopping the other checks.

### Key Entities

- **Check**: one configured or discovered thing to watch — its type, name, schedule, threshold, delivery mode, whether it needs the internet, optional dependency, and type-specific settings.
- **Condition**: a single judged fact under a check (for example "drive X wear above limit", "container Y down"); has a key, a current verdict, the start of its broken streak, the last bad reading, and whether a PROBLEM is open.
- **Baseline**: per-drive reference values (error counters, last self-test seen) keyed by serial, used to detect rises.
- **Alert record**: a PROBLEM, RESOLVED, retirement or fix action with time, check, condition key and text; forms the alert log.
- **Sample**: a numeric reading (sensor, drive or UPS metric) with time; raw or daily roll-up.
- **Probe state**: the modem test's last sent token and time, and the time of the last successful return.
- **Heartbeat**: the liveness record an external watchdog reads — last cycle time and planned-reboot end.
- **Queued alert**: a message that could not be delivered yet and waits for connectivity.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: On a real host, the one-shot mode reports every configured check with a reading and verdict that match what the operator observes by hand, with zero messages sent.
- **SC-002**: For every behaviour listed in the acceptance scenarios, an automated test exists and passes; the testable parts of the feature meet the project's existing coverage floor (80%).
- **SC-003**: A self-healing failure shorter than its threshold produces zero messages; a lasting failure produces exactly one PROBLEM and one RESOLVED, including across a daemon restart in between.
- **SC-004**: A network outage of any length produces exactly one SMS PROBLEM and one RESOLVED, and no other alerts caused by the outage.
- **SC-005**: Swapping a drive, adding a GPU or adding a container with a restart policy requires no configuration change to be monitored.
- **SC-006**: The external watchdog sees a heartbeat no older than one check cycle plus one minute whenever the daemon is running, including during maintenance windows.
- **SC-007**: `/status`, `/history`, `/temp` and `/disks` reply within 5 seconds.
- **SC-008**: After switch-over, the operator can retire the old cron health checkers with no loss of any alert they produced (excluding the intentionally dropped SMS reboot/ping commands).
- **SC-009**: A search of the repository for the operator's real host names, addresses, phone numbers, container names and drive serials finds nothing.

## Assumptions

- The daemon runs directly on the monitored host with root privileges (hardware and kernel-log access). Running hardware checks from the container image is not supported.
- The host's standard disk-health tool (smartmontools) is installed; GPU readings use the vendor's command-line tool when present. Both are documented host requirements.
- Distribution of work into the five phases matches the user stories' priorities; User Story 1 includes the network and UPS checks so it is useful on its own.
- Default thresholds follow today's behaviour: 15 minutes for containers, immediate for drive health changes and modem probe failures, 30 minutes for the probe's return, 8 days without a returned probe, 10 minutes of boot grace, 6 hours between "still happening" kernel messages.
- Incoming calls, the UPS forced-shutdown path, maintenance and upgrade scheduling, backups and the external watchdog itself stay outside tgshd; tgshd only reads their lock files and stamps and serves the heartbeat.
- The SMS reboot and SMS ping commands of the old setup are intentionally not carried over.
- Only one tgshd instance runs per host.
- Email for checker errors goes through an SMTP relay the operator configures (typically the host's local mail transfer agent); no mail server is part of tgshd.
- The planned-reboot command (FR-017) is run locally by the operator's reboot script and only touches the heartbeat file; it is not a general control channel into the daemon.
- New third-party dependencies are approved by the maintainer as part of planning (pure-Go storage, container-runtime client, YAML parsing, DNS message encoding); the static, no-cgo build stays.
