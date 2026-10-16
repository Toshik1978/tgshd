# Contract: Command line

| Invocation | Behaviour | Exit |
|---|---|---|
| `tgshd` | Run the daemon (unchanged). Health monitoring runs when `BOT_HEALTH_CONFIG` is set. | as today |
| `tgshd check --once` | Load env + health config, run every configured check once with real adapters, print one line per reading: `<VERDICT> <key>: <text>` and samples, then a summary line. Sends nothing, writes nothing (state DB opened read-only if present, to show baseline deltas). | 0 ran; 1 config/setup error |
| `tgshd heartbeat --planned-reboot <seconds>` | Set `planned_until = now + seconds` in the heartbeat file (0 clears) under the heartbeat lock. Runs no checks. | 0 ok; 1 bad argument or write error |
