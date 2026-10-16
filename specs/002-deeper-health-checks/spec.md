# Feature Specification: Deeper Health Checks

**Feature Branch**: `002-deeper-health-checks`

**Created**: 2026-10-16

**Status**: Draft

**Input**: User description: "Deeper health checks: extend Host Health Monitoring (001) so that tgshd's monitoring is complete enough to be the host's only monitor, with drive failure as the top concern."

Extends host health monitoring (001) with deeper drive checks (more error counters, a spare floor, calendar self-test age, sleeping drives), fixes to how problems resolve and get delivered, richer kernel-error coverage, quieter temperature and network alerting, more expressive service checks, and a richer weekly summary.

This feature builds on [001 Host Health Monitoring](../001-host-health-monitoring/spec.md). Where a
requirement here changes a 001 requirement it says so ("amends 001 FR-nnn"); everything in 001 that
is not amended stays in force. Each requirement states why it is needed, so that it can be judged
on its own merits.

## Clarifications

### Session 2026-10-16

- Q: While Telegram is down and PROBLEMs go out by SMS, should the matching RESOLVED also go by SMS? → A: Yes, RESOLVED goes by SMS only for PROBLEMs that were themselves sent by SMS.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Drive failure is caught early and never hidden (Priority: P1)

The operator relies on tgshd as the only thing watching the host's drives. tgshd tracks the full
set of error counters that signal a drive is losing data, alerts when NVMe spare capacity runs
low, measures self-test age in calendar days, and reads every drive often enough that its health
is never unknown for long, without waking sleeping drives on every check. A read that comes back
incomplete never makes an open drive problem look solved.

**Why this priority**: Drive failure loses data, and data loss is the most expensive thing that
can happen to the host. Every other improvement here is secondary to catching a dying drive.

**Independent Test**: Feed recorded drive reads (healthy; a rise in each default counter; spare
at 19%; a read with no health verdict; a read missing its attribute table; a drive asleep for
25 hours; a drive whose last passed self-test is 41 days old) and verify the alerts, checker
errors, kept state and forced read.

**Acceptance Scenarios**:

1. **Given** a SATA drive whose baseline shows attribute 183 at 0, **When** a read shows 1, **Then** one alert names the drive and the rise; **When** a later read shows 3, **Then** one more alert is sent for that rise.
2. **Given** a drive with an open "health failing" problem, **When** a read returns no health verdict, **Then** the problem stays open, no RESOLVED is sent, and a checker error is raised.
3. **Given** an NVMe drive whose own spare threshold is 10%, **When** its available spare drops to 19%, **Then** a spare problem opens (the 20% floor is reached first).
4. **Given** a drive that is asleep on every check, **When** its last successful read becomes older than 24 hours, **Then** the next check reads it even if that wakes it, and `/disks` shows how long ago it was last read.
5. **Given** a drive whose last passed self-test was observed 41 days ago, **When** the disks check runs, **Then** a stale self-test problem opens.
6. **Given** a drive first seen today with no passed self-test on record, **When** the disks check runs during the next 40 days, **Then** no stale self-test problem opens.
7. **Given** a drive's self-test log gains a new failed entry and then a newer passing entry before the next check, **When** the check runs, **Then** the failure is reported once.
8. **Given** a USB-attached drive, **When** discovery runs, **Then** it is skipped unless the operator has opted that drive in.
9. **Given** drives are known, **When** a discovery finds no drive at all, **Then** a checker error is raised and no "drive missing" problem opens.
10. **Given** an NVMe drive whose error-information-log entry count rises, **When** the check runs, **Then** no alert is sent.

---

### User Story 2 - Problems are delivered and resolved correctly (Priority: P2)

The operator trusts that a PROBLEM reaches them even when Telegram is unreachable, that it is
never lost on the way, and that it is resolved only by a reading that really shows the condition
is fine. Maintenance and boot-grace windows pause only what they are meant to pause, and a
maintenance lock that was never released does not silence monitoring forever.

**Why this priority**: A correct alert that never arrives, or that resolves itself because of a
bad reading, is as harmful as no alert. This story protects every other check.

**Independent Test**: With fake Telegram, SMS and email channels, make Telegram fail for 20
minutes while a PROBLEM is raised, drop a PROBLEM from the retry queue, hold the maintenance lock
for 7 hours while a drive counter rises, and raise two different checker errors from one check;
verify SMS and email delivery, the re-raise, the hardware alert during maintenance, the stuck-lock
error and both checker errors.

**Acceptance Scenarios**:

1. **Given** Telegram delivery has been failing for 15 minutes, **When** the next cycle runs, **Then** one checker error is sent by email naming Telegram as the failing channel, and no second one while the failure lasts.
2. **Given** Telegram delivery is failing, **When** a PROBLEM is queued, **Then** it is also sent by SMS; **When** that condition resolves while Telegram is still failing, **Then** its RESOLVED is also sent by SMS.
3. **Given** a PROBLEM is dropped from the retry queue without being delivered, **When** the next cycle runs, **Then** if its condition is still open it is reopened, so the PROBLEM is raised again while the condition stays broken; if the condition has resolved or has started a new streak, the PROBLEM is listed in a later delivered message and kept until it has been shown.
4. **Given** the maintenance lock is held, **When** a drive error counter rises, **Then** the alert is sent; **When** a container goes down during the same window, **Then** it is not judged.
5. **Given** the maintenance lock has been held for more than 6 hours, **When** a cycle runs, **Then** a checker error says the lock has been held too long.
6. **Given** one check raised checker error A less than 24 hours ago, **When** the same check raises a different error B, **Then** B is delivered.
7. **Given** state writing failed and was reported, **When** writing succeeds again, **Then** one recovery notice is sent.

---

### User Story 3 - Complete kernel and memory error coverage (Priority: P3)

Hardware errors that the kernel or firmware reports reach the operator, including those logged
just before a crash or power cut, those reported by firmware rather than the kernel's own
drivers, and every uncorrectable memory error.

**Why this priority**: Kernel and firmware records are often the only early sign of failing memory,
CPU, controllers or cables, and the records written just before a crash are the ones that explain
it.

**Independent Test**: Feed recorded kernel records of each class (firmware hardware error,
machine-check summary, filesystem I/O error, SATA link reset) and a previous-boot journal tail;
feed memory-controller counters with an uncorrectable rise and one unreadable controller; verify
the messages and checker errors.

**Acceptance Scenarios**:

1. **Given** a hardware error was logged in the previous boot after the last record tgshd processed, **When** the daemon starts after the reboot and the persistent journal is available, **Then** the error is reported.
2. **Given** a firmware-reported "[Hardware Error]" record, a filesystem "I/O error" record, or a SATA link reset at any log level, **When** it is read, **Then** it is counted in its class and reported.
3. **Given** a kernel error class has pending counts, **When** its 6-hour window passes with no new record in that run, **Then** a "still happening" message with the counts is sent.
4. **Given** the uncorrectable memory error count rises twice within an hour, **When** each rise is read, **Then** each is reported at once.
5. **Given** one memory controller's counter cannot be read, **When** the kernel check runs, **Then** the other controllers are still judged and a checker error is raised for the unreadable one.
6. **Given** kernel records were lost before they could be read, **When** the loss is detected, **Then** a checker error states how many were lost.

---

### User Story 4 - Quieter, accurate temperature alerting (Priority: P4)

Temperature alerts fire on a real overheating and stay silent for noise: one condition per CPU
package rather than per core, board sensors only when they mean something, and no messages about
sensors that disappeared without ever having had a problem. A sensor that cannot be read is
treated as a checker problem, never as "gone", so an open overheating problem is not resolved by
a failed read.

**Why this priority**: Noisy alerts train the operator to ignore them; a missed overheating
alert lets hardware degrade. Both matter, after drives and delivery.

**Independent Test**: Feed recorded sensor readings: per-core and per-package CPU values, an
unlisted board sensor with and without a critical limit, a drive at exactly 60 °C, a sensor that
stops reading while its problem is open, a GPU that stops reporting, and a sensor that disappears
without an open problem; verify alerts, kept state and messages.

**Acceptance Scenarios**:

1. **Given** default limits, **When** an NVMe drive reads 60 °C for the first time, **Then** a PROBLEM is sent at once.
2. **Given** a CPU with one package and 16 cores, **When** a core runs hot, **Then** judging uses the package sensor, and at most one CPU condition exists for that package.
3. **Given** a board sensor the operator did not list and that exposes no critical limit, **When** it reads high, **Then** nothing is sent.
4. **Given** a sensor with an open problem, **When** it can no longer be read, **Then** a checker error is raised, the problem stays open, and no "gone" message is sent.
5. **Given** a GPU seen before, **When** the vendor tool reports no devices, **Then** a checker error is raised and the GPU's state is kept.
6. **Given** a sensor without an open problem, **When** its device disappears from a complete read, **Then** its state is dropped and no message is sent.
7. **Given** a per-serial limit of 50 °C for one drive, **When** that drive reads 52 °C, **Then** a PROBLEM is sent while other drives use the class limit.

---

### User Story 5 - Network and service checks that judge what matters (Priority: P5)

The operator expresses what "up" means for each service precisely: whether a redirect is up,
what a probe's output must or must not contain, that a backup stamp from the future is not
fresh, and that a name the resolver does not know is down. A network outage is one problem from
start to end, and losing the default route counts as the router being down.

**Why this priority**: These are correctness fixes for checks that already exist; they remove
false "up" verdicts and duplicate alerts, but each affects one check type.

**Independent Test**: With fake network, endpoint, DNS-over-HTTPS, command and file sources,
simulate a missing default route, an outage whose cause moves from router to internet, a
redirect, a probe whose output contains a forbidden pattern, a stamp one day in the future, and
a no-such-name answer; verify the verdicts and messages.

**Acceptance Scenarios**:

1. **Given** the host has no usable default route, **When** the network check runs for one interval, **Then** the router is judged down and the PROBLEM goes by SMS.
2. **Given** an outage that starts at the router and continues as an internet outage, **When** it ends, **Then** exactly one PROBLEM and one RESOLVED were sent.
3. **Given** an endpoint check with default settings, **When** the endpoint answers with a redirect to example.org, **Then** the redirect is not followed and the first response is judged.
4. **Given** a command check with a forbidden output pattern, **When** the probe exits 0 but prints that pattern, **Then** the check is judged broken.
5. **Given** a freshness stamp dated a day in the future, **When** the check runs, **Then** it is judged stale.
6. **Given** an endpoint check using DNS-over-HTTPS, **When** the resolver answers that the name does not exist, **Then** the endpoint is judged down; **When** the resolver cannot be reached, **Then** the reading is unreadable.
7. **Given** a freshness check reading its timestamp from an HTTP endpoint, **When** the endpoint does not answer, **Then** the reading is unreadable and no problem opens or resolves.

---

### User Story 6 - A weekly summary that shows the whole host (Priority: P6)

The weekly summary lets the operator confirm, in one message, that every drive is readable and
tested, every service and backup is current, and the SMS fallback works.

**Why this priority**: The summary catches slow drift that never crosses an alert threshold; it
adds value but does not alert on its own.

**Independent Test**: Seed state with drives (one asleep, one unreadable, one SATA SSD reporting
attribute 177), mounts, freshness stamps of various ages and a modem probe result; build the
summary with and without a length overflow; verify each line and the cut order.

**Acceptance Scenarios**:

1. **Given** a drive with two mounted partitions, **When** the summary is built, **Then** its line shows the device node, both mount points, the current temperature, the newest self-test result and the age of the last passed test.
2. **Given** a drive asleep at the last attempt since Tuesday, **When** the summary is built, **Then** its line is marked asleep and says since when.
3. **Given** a SATA SSD reporting a normalised attribute 177 of 92, **When** the summary is built, **Then** its line shows 8% wear.
4. **Given** a freshness stamp 30 hours old, **When** the summary is built, **Then** that check's line shows "30 h" ago.
5. **Given** the modem probe last came back 3 days ago, **When** the summary is built, **Then** it shows that age and whether a probe is outstanding.
6. **Given** the summary exceeds the message limit, **When** it is cut, **Then** the services section is shortened first and the open-problems section is kept whole as long as it fits.

---

### Edge Cases

- A drive sleeps through every check and the forced read fails: the failure is a checker error, the drive is marked unreadable in the summary, and its open problems stay open.
- A drive reports a default counter that climbs during normal operation: every rise alerts, by design; the operator's recourse is excluding the drive (see Assumptions).
- A drive's read lacks the self-test log section only: counters and health are still judged; self-test state is kept and a checker error is raised.
- Telegram and email both fail during a network outage: the email failure is logged; PROBLEMs still go by SMS; nothing is dropped silently.
- SMS is not configured while Telegram is down: PROBLEMs stay queued; the email checker error (when email is configured) is the only notice.
- The maintenance lock is held through a reboot: the boot-grace window and the maintenance window both pause only the checks set to pause in quiet windows (`pause_in_quiet_windows`); hardware checks keep running.
- No persistent journal exists: the previous boot's tail cannot be read; this is written to the log at start-up and checking resumes from the current boot.
- A board sensor reports an implausible value (for example −273 °C or 1000 °C): it is unreadable, not hot.
- A freshness stamp is a few minutes in the future because of clock skew between the writer and the host: within the tolerance it is fresh.
- An outage ends and starts again within one interval: it is one outage only if no good reading was taken in between.
- The weekly summary's open-problems section alone exceeds the limit: it is cut with an ellipsis as the last resort, never dropped.

## Requirements *(mandatory)*

### Functional Requirements

**Drive discovery and reads**

- **FR-001** (amends 001 FR-020): Drive discovery MUST include only drives on internal transports (NVMe, SATA, SAS) and MUST skip USB-attached and other hot-pluggable drives unless the operator opts a drive in by serial. A drive already tracked that now classifies as USB-attached or other hot-pluggable and is not opted in MUST stop being tracked silently (with a log line), never reported missing. *Why*: an external drive that is unplugged after use would otherwise open a "drive missing" problem every time it leaves.
- **FR-002**: A discovery that finds no drive at all while drives are known MUST raise a checker error and MUST NOT open "drive missing" problems. *Why*: every drive vanishing at once almost always means the reading tool or its permissions failed, not the hardware.
- **FR-003**: Routine reads MUST NOT wake a sleeping drive; a drive found asleep is skipped for that run with all its state kept. When a drive's last successful read is older than a configurable age (default 24 h), the system MUST read it even if that wakes it; a forced read, successful or not, MUST NOT be repeated for that drive before the same age has passed again. The time since the last successful read MUST appear in the drive's status. *Why*: needless spin-ups cost power and wear, but a drive that is never read could fail unseen.
- **FR-004** (amends 001 FR-021): A drive's overall health MUST be judged only from an explicit health verdict in the read. A read without a verdict MUST leave the health condition unchanged, MUST NOT resolve an open health problem, and MUST raise a checker error. *Why*: a partial read says nothing about health, and resolving a failing-drive problem on such a read would hide the most important alert.
- **FR-005**: When a drive's read lacks a section its protocol needs (ATA attribute table, NVMe health log, self-test log), the system MUST raise a checker error for that drive and keep the drive's previous state for every condition that section feeds. *Why*: a missing section is a reading failure, and treating it as "all counters gone" or "no self-test" would produce false alerts or false recoveries.
- **FR-006** (amends 001 FR-021): The system MUST track a built-in set of drive error counters: ATA attributes 5 (reallocated sectors), 179 (used reserved blocks), 181 (program fail), 182 (erase fail), 183 (runtime bad blocks), 187 (reported uncorrectable), 188 (command timeout), 195 (hardware ECC), 197 (pending sectors), 198 (offline uncorrectable) and 199 (interface CRC errors), and the NVMe media and data-integrity error count. Any rise against the drive's own baseline MUST alert immediately, one alert per rise. The operator MUST be able to add counters (ATA attribute ids) globally or per drive serial, and MUST NOT be able to remove a built-in counter. *Why*: each of these counts data the drive could not read, write or move intact, and a rise in any of them is an early sign of failure; comparing with the drive's own baseline means a counter that was already non-zero does not alert until it grows, so a built-in counter never needs removing.
- **FR-007** (amends 001 FR-021): The NVMe error-information-log entry count MUST NOT raise alerts; it MAY be sampled for history. *Why*: the log records benign events (such as commands the drive does not support), so its rises are noise rather than signs of failure.
- **FR-008** (amends 001 FR-021): An NVMe drive's available spare MUST open a problem when it reaches the drive's own spare threshold or falls below a configurable floor (default 20%), whichever happens first. When the spare is at or below the drive's own threshold the message names that threshold; otherwise it names the floor. *Why*: many drives set their own threshold low (often 10% or less), which leaves too little time to buy and fit a new drive.
- **FR-009**: For SATA SSDs that report attribute 177 (wear levelling), the system MUST derive wear as 100 minus its normalised value, show it in `/disks` and the weekly summary, and judge it against the configured wear level, as NVMe percentage used already is. *Why*: wear is the main end-of-life signal of an SSD, and SATA SSDs otherwise have none.
- **FR-010** (amends 001 FR-022): Self-test results MUST be classified as passed, aborted, in progress or failed, with every unrecognised result code counted as failed. Every failure that newly appears in a drive's self-test log MUST be reported once, even when a newer test has passed since. *Why*: an unknown code is safer read as a failure, and a failure followed by a pass is still a drive that failed a test.
- **FR-011** (amends 001 FR-022): Self-test age MUST be measured in calendar time since a passed self-test was last newly observed, persisted per drive, with a default maximum of 40 days. A newly seen drive MUST count from when it was first seen. *Why*: operators schedule self-tests by the calendar, and power-on time runs slowly on drives that sleep or hosts that are often off; 40 days allows a monthly schedule plus margin, and the first-sighting grace keeps a new drive from alerting on its first check.
- **FR-012**: A drive whose self-test log format is not recognised MUST NOT be judged for self-tests, and its status MUST say that self-tests cannot be read for it. SAS drives' self-test logs are not read in this feature and always count as not recognised (no checker error). *Why*: judging an unreadable log would report "no test" for a drive that may well be tested.
- **FR-013**: A drive whose read yields no temperature MUST raise a checker error (rate-limited per FR-028). *Why*: otherwise a drive's temperature would go silently unmonitored.

**Temperatures**

- **FR-014** (amends 001 FR-025): Default temperature limits MUST be NVMe drives 60 °C, SATA/SAS drives 60 °C, CPU package 90 °C and GPU 85 °C. A reading at or above its limit MUST count as broken, and the default threshold MUST be 0 (the first such reading opens a problem); the threshold stays configurable. *Why*: 60 °C sits at the top of the operating range most drive specifications allow; 90 °C lets a CPU boost under normal load but alerts before it reaches its throttling point; 85 °C for GPUs is unchanged from 001. Temperatures move slowly, so waiting for a streak delays the alert without filtering real noise.
- **FR-015**: Temperature limits MUST be overridable per class and per drive serial. *Why*: drives differ in their rated maximum, and one hot-running drive should not force a looser limit on all the others.
- **FR-016**: CPU temperature MUST be judged per package or die sensor, with one condition per package, never per core. When the temperatures check is enabled and no CPU sensor is found, the system MUST raise a checker error. *Why*: per-core readings duplicate one fact many times, and a missing CPU sensor means the most important temperature is not watched.
- **FR-017**: Board sensors MUST be judged only for chips and labels the operator lists, or against the critical limit a sensor exposes itself. A reading outside the plausible physical range MUST be unreadable. *Why*: unlabelled board sensors often report meaningless values, and a value no hardware could reach is a reading fault, not heat.
- **FR-018** (amends 001 FR-027): A sensor that is present but cannot be read, and a GPU that was seen before but for which the vendor tool reports no device or no temperature, MUST be unreadable: a checker error is raised and its state, including an open problem, is kept. Only a sensor whose device disappeared from an otherwise complete read may be reported gone. *Why*: a failed read says nothing about the temperature, and treating it as "gone" would silently resolve an overheating problem.
- **FR-019** (amends 001 FR-027): A "gone" message MUST be sent only when the gone sensor had an open problem; otherwise its state MUST be dropped without a message. *Why*: a sensor that was fine and then left is not news, and such messages are noise after every hardware change.

**Kernel and memory errors**

- **FR-020** (amends 001 FR-026): After a reboot, the system MUST read kernel records logged in the previous boot after the last record it processed, from the persistent system journal when one is available, and report the hardware errors among them. When no persistent journal is available, it MUST log that the previous boot's tail could not be checked. When the tail cannot be read completely within the check's time or record limit, it MUST raise a checker error and move on to the current boot after that one attempt. *Why*: errors written just before a crash or power cut usually explain it and would otherwise never be seen.
- **FR-021** (amends 001 FR-026): The kernel error classes MUST also match firmware-reported hardware errors (APEI/GHES "[Hardware Error]" records anywhere in a record) and machine-check summary lines as machine-check errors, "I/O error" anywhere in a record from any storage or filesystem subsystem as storage I/O errors, and SATA link resets and SError reports at any log level as ATA errors. *Why*: these records report the same failing memory, controllers and cables as the classes already matched, but in wording or at log levels the earlier patterns missed.
- **FR-022** (amends 001 FR-026): Pending kernel-error counts MUST be reported in a "still happening" message once the 6-hour window has passed, even when no new record arrives in that run. *Why*: otherwise counts gathered in the window would wait for an unrelated later record, or never be reported.
- **FR-023**: When kernel records are lost before they could be read, the system MUST raise a checker error stating how many were lost. *Why*: lost records may have been hardware errors, so the operator must know the coverage had a gap.
- **FR-024** (amends 001 FR-026): Every rise of an uncorrectable memory error count MUST be reported at once; episode throttling MAY apply to correctable counts only. *Why*: each uncorrectable memory error may have corrupted data or crashed a process, so none may be folded into a later summary.
- **FR-025**: One unreadable memory-controller counter MUST NOT prevent judging the other controllers; it MUST raise a checker error for that controller. *Why*: one bad counter file should not blind the check to errors on every other controller.

**Quiet windows, delivery and resolution**

- **FR-026** (amends 001 FR-005 and FR-006): Maintenance and boot-grace windows MUST NOT pause hardware checks (drive health, temperatures, kernel and memory errors) unless the operator sets the check to pause in quiet windows (`pause_in_quiet_windows`); all other checks keep 001's behaviour. *Why*: maintenance and reboots do not stop hardware from failing, and a reboot caused by failing hardware is exactly when those checks matter.
- **FR-027**: A maintenance lock held longer than a configurable maximum (default 6 h) MUST raise a checker error. *Why*: a lock left behind by a crashed job would otherwise silence monitoring indefinitely; 6 hours covers a long upgrade or backup run.
- **FR-028** (amends 001 FR-045): Checker-error rate limiting MUST be per distinct error (check, subject and kind of error), not per check. *Why*: a new error must reach the operator even while a different error of the same check is inside its 24-hour window.
- **FR-029**: When Telegram delivery keeps failing for longer than a configurable time (default 15 min), the system MUST send one checker error by email naming the failing channel and since when it has been failing. *Why*: Telegram is the main channel, and its silent failure would otherwise hide every alert until someone notices.
- **FR-030** (amends 001 FR-009): While Telegram delivery is failing, the system MUST also deliver each queued PROBLEM by SMS, and the RESOLVED of a PROBLEM that was sent by SMS MUST also go by SMS; other messages (events, RESOLVED of PROBLEMs not sent by SMS) wait for Telegram. *Why*: a PROBLEM waiting in a queue does not reach the operator, SMS does not depend on Telegram, and an operator alerted by SMS must also learn by SMS that the problem cleared.
- **FR-031**: An undelivered PROBLEM MUST never be dropped silently: when it leaves the retry queue undelivered and its condition is still open, the condition MUST be left not open, so that the PROBLEM is raised again while the condition stays broken; when its condition has resolved or has started a new streak, the PROBLEM MUST be listed in a later delivered message. A listed PROBLEM MUST be kept until it has been shown in a delivered message; lines that do not fit a message wait for the next one, and PROBLEMs beyond the list's capacity MUST be counted in a line that is always shown. *Why*: a condition marked open whose PROBLEM never arrived would never alert again, and a PROBLEM that is only logged never reaches the operator.
- **FR-032** (amends 001 FR-013): When state writing recovers after a reported failure, the system MUST send one recovery notice. *Why*: the operator was told persistence was broken and needs to know it works again.

**Network**

- **FR-033** (amends 001 FR-008): No usable default route MUST be judged as the router being down and alerted by SMS, not as an unreadable reading. *Why*: without a route the host is offline as surely as when the router does not answer, and an unreadable verdict would never alert.
- **FR-034** (amends 001 FR-008): A network outage MUST be one condition with one PROBLEM and one RESOLVED, however its cause (router or internet) changes during the outage. *Why*: a cause that shifts mid-outage is still one outage, and extra messages cost SMS and attention.
- **FR-035**: The network check's default threshold MUST be one check interval. *Why*: a single lost round of pings is common and heals by itself, while an outage confirmed on the next cycle is real.
- **FR-036** (amends 001 FR-009): While the network reading is unreadable, checks that need the internet MUST NOT be judged. *Why*: their failures could be caused by the network, and judging them would page for the wrong cause.

**Service checks**

- **FR-037** (amends 001 FR-030): Endpoint (http) checks MUST NOT follow redirects by default and MUST judge the first response; following redirects MUST be configurable per check. *Why*: a redirect to a login or error page can make a broken service look up, and judging the first response lets "any 2xx–3xx answer is up" be stated exactly.
- **FR-038** (amends 001 FR-030): An endpoint check whose linked container does not exist MUST be judged unhealthy, not unreadable. *Why*: a container that no longer exists is a service that is down.
- **FR-039** (amends 001 FR-030 and FR-045): An endpoint check MUST be able to treat authentication refusals (401 and 403) as a checker error rather than the service being down, and the operator MUST be able to configure extra placeholder markers that identify a secret that was never deployed. *Why*: a wrong or missing credential is a configuration fault, not an outage, and must not page as one.
- **FR-040** (amends 001 FR-030): A DNS-over-HTTPS answer that the name does not exist or has no address MUST be judged as the endpoint being down; only a failure to reach the resolver is unreadable. *Why*: a name that does not resolve makes the service unreachable for every user.
- **FR-041** (amends 001 FR-033): Command checks MUST be able to judge the probe's output, with an expected pattern (must appear) or a forbidden pattern (must not appear), in addition to its exit status, and the operator MUST be able to choose whether a timeout counts as broken (default) or unreadable. *Why*: many probes report trouble in their output while exiting 0, and some probes time out for reasons that are not the service's fault.
- **FR-042** (amends 001 FR-031): A freshness timestamp more than a configurable tolerance (default 1 h) in the future MUST be judged stale. *Why*: a future stamp comes from a broken clock or writer, and treating it as fresh would hide a failing backup until that future date.
- **FR-043** (amends 001 FR-031): The system MUST support freshness checks whose timestamp comes from an HTTP endpoint's response (a configured request, an optional request header whose value comes from a secret in the environment, a response holding a Unix time), judged against a maximum age, and unreadable while the endpoint does not answer. *Why*: some jobs report their last success through a service rather than a file on the host.

**Weekly summary**

- **FR-044** (amends 001 FR-040): Each drive line of the weekly summary MUST show the device node, the mount points of the drive and its partitions ("-" when none, "?" when unknown), the current temperature, the newest self-test result, the calendar age of the last passed test, and wear (including SATA SSD wear per FR-009). *Why*: the operator can match each line to a physical drive and its data and see at a glance that it is cool, tested and not wearing out.
- **FR-045** (amends 001 FR-040): The weekly summary MUST mark each drive that was asleep or could not be read at the last attempt, and since when. *Why*: a drive with no fresh reading must not look healthy.
- **FR-046** (amends 001 FR-040): The services block MUST have one line per configured check, or per operator-defined group, reading "ok" or the open problems' texts, including the network, UPS and modem probe. *Why*: one line per check shows that each one is running, not only those that failed.
- **FR-047** (amends 001 FR-040): Each freshness check's summary line MUST show the age of its timestamp ("<1 h", "N h" or "N d" ago; "none" when missing; "?" when unreadable). *Why*: backup stamp ages show a job drifting late before it crosses its maximum age.
- **FR-048** (amends 001 FR-040): The weekly summary MUST show the modem probe's last successful round trip (its age, or "never") or its failure text, and whether a probe is outstanding. *Why*: the SMS fallback is only trustworthy if it is seen to work.
- **FR-049** (amends 001 FR-040): When the weekly summary exceeds the message limit, the services section MUST be shortened first and the open-problems section MUST be cut last, kept whole as long as it fits. *Why*: open problems are the part of the summary that needs action.

### Key Entities

- **Drive status**: per drive (by serial) — device node, mount points, last successful read, asleep since, unreadable since, current temperature, self-test format supported or not.
- **Drive baseline** (extends 001 Baseline): the built-in and operator-added error counters the drive reports, the time a passed self-test was last newly observed, the first-seen time used for self-test grace, and the self-test failures already reported.
- **Counter set**: the built-in error counters plus operator additions, global or per drive serial.
- **Temperature limit**: a limit per class (NVMe, SATA/SAS, CPU, GPU, listed board sensor) with optional per-serial overrides for drives, and the streak threshold.
- **Channel health**: per delivery channel, since when delivery has been failing and whether the failure was reported.
- **Undelivered PROBLEM**: a PROBLEM that left the retry queue without delivery and whose condition is no longer open, kept until it has been listed in a delivered message (or counted in its overflow line).
- **Kernel cursor** (extends 001 KernelCursor): the last processed record, kept across boots so the previous boot's tail can be read.
- **Freshness source**: a file (timestamp content or modification time) or an HTTP endpoint returning a Unix time.
- **Maintenance lock observation**: since when the lock has been continuously held.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For each built-in drive error counter, a rise of 1 in a recorded read produces exactly one alert within one disks check interval.
- **SC-002**: No open drive or temperature problem is resolved by a read that lacks a health verdict, lacks the relevant section, or fails; verified for every such case in the acceptance scenarios.
- **SC-003**: Over a simulated week with drives asleep at every check, each drive is woken by tgshd at most once per 24 hours.
- **SC-004**: With Telegram unreachable for 20 minutes, every PROBLEM raised in that time reaches the operator by SMS, and exactly one email naming Telegram arrives within 15 minutes plus one cycle.
- **SC-005**: Of PROBLEMs that leave the retry queue undelivered, 100% are raised again, listed in a later delivered message, or counted in that message's overflow line; none is only logged.
- **SC-006**: A hardware fault occurring during a maintenance or boot-grace window is alerted within one check interval, unless the operator set that check to pause in quiet windows (`pause_in_quiet_windows`).
- **SC-007**: A hardware error logged in the minute before an unclean reboot is reported within one kernel check interval after the daemon starts, on a host with a persistent journal.
- **SC-008**: A network outage whose cause changes any number of times produces exactly one PROBLEM and one RESOLVED.
- **SC-009**: Over a simulated week of normal sensor readings, zero alerts come from per-core CPU sensors, unlisted board sensors without a critical limit, or sensors that disappeared without an open problem.
- **SC-010**: Every weekly summary is one message within the length limit, and it contains the open-problems section in full whenever that section alone fits.
- **SC-011**: For every acceptance scenario an automated test exists and passes, and the project's coverage floor (80%) is met.
- **SC-012**: Every failure class this feature covers (each drive counter, spare, wear, self-test, temperature class, kernel class, memory errors, network, each service check type, delivery failure) has at least one test with recorded input that ends in an alert, so the operator can run tgshd as the host's only monitor.

## Assumptions

- All 001 assumptions stay in force (root on the host, the standard disk-health tool installed, one instance per host).
- Out of scope: boot or "host online" announcements, incoming calls, UPS shutdown handling, and maintenance or upgrade scheduling and backups themselves; tgshd only reads their stamps and locks.
- The NVMe error count in the built-in set is the drive's media and data-integrity error count; attribute 252 (added bad blocks) and other vendor-specific attributes are not in the built-in set and can be added by the operator.
- A drive on which a built-in counter climbs during normal operation alerts on every rise, by design; the operator's recourse is excluding that drive from the disks check. Built-in counters cannot be turned off individually.
- "Asleep" means the drive reports a standby or sleep power state when asked without waking it; drives that cannot report their power state are read normally.
- The plausible physical range for temperature readings is −40 °C to 150 °C. It applies to drive temperatures too, where 0 °C (smartctl's value for a drive without a reading) is also unreadable: such a reading is not sampled and neither opens nor resolves the drive's temperature problem.
- The email checker error of FR-029 needs an SMTP relay; without one, the Telegram failure is written to the log and PROBLEMs still go by SMS when SMS is configured. A PROBLEM sent by SMS under FR-030 is still delivered to Telegram when it recovers, so the chat history stays complete.
- Under FR-031 the system prefers leaving the condition not open (so it is raised again); listing in a later message is used for PROBLEMs whose condition has meanwhile resolved or started a new streak.
- The freshness future tolerance (1 h) absorbs clock differences between the host and a stamp writer elsewhere; it is configurable per check.
- Whether a check pauses in maintenance and boot-grace windows is a per-check setting (`pause_in_quiet_windows`); hardware checks do not pause by default.
- New third-party dependencies, if any, are approved by the maintainer during planning; the static, no-cgo build stays.
