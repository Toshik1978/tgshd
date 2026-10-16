# Contract: Heartbeat file

- Path: `general.heartbeat_file` (default `/run/tgshd/heartbeat`); directory mode 0755, file 0644.
- Content: exactly one line, `ts=<epoch> planned_until=<epoch>\n`; `planned_until=0` means none.
- Written every tick (30 s) by the daemon, including ticks inside quiet windows; `ts` is the write
  time.
- Writers (daemon, `tgshd heartbeat`) take an exclusive `flock` on `<path>.lock`, read the
  current `planned_until`, write a temp file in the same directory and `rename` it over the path.
- The daemon keeps `planned_until` until it has passed, then writes 0.
- Readers (an external watchdog, typically served by a local web server) treat a missing file or
  an old `ts` as "daemon quiet", unless `now < planned_until`.
