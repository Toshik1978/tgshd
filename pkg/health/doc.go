// Package health is the pure alert engine of host health monitoring: readings,
// conditions, thresholds counted over observed time, quiet windows and rate
// limits. It performs no I/O; checks, the runner, storage and notification
// build on it, while host adapters live under pkg/host.
package health
