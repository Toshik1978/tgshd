# Contract delta: Command line

**Amends**: [001 contracts/cli.md](../../001-host-health-monitoring/contracts/cli.md). The
invocations and exit codes are unchanged; `tgshd heartbeat` and the
[heartbeat file contract](../../001-host-health-monitoring/contracts/heartbeat.md) are unchanged.

| Invocation | Change |
|---|---|
| `tgshd check --once` | Never wakes a sleeping drive: a drive in standby prints `ASLEEP disks/<serial>: last read <age> ago` (the daemon's forced read, FR-003, is not done). |
| `tgshd check --once` | Prints every distinct checker error of a run on its own line `ERROR <check>/<subject>/<kind>: <text>` (FR-028). |
| `tgshd check --once` | When the state database is still at schema version 1 (the daemon has not been restarted since the upgrade), runs without stored state and prints `state database is at schema version 1; run the daemon once to migrate` with exit 0. A database newer than the build keeps the 001 behaviour: a warning on stderr, the run goes on without stored state, exit 0. |
| `tgshd check --once` | The kernel one-shot does not read the previous boot's journal tail (FR-020 is a start-up action of the daemon). |
