package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"sync"
	"time"

	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/doh"
	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/command"
	"github.com/Toshik1978/tgshd/pkg/health/check/containers"
	"github.com/Toshik1978/tgshd/pkg/health/check/freshness"
	"github.com/Toshik1978/tgshd/pkg/health/check/httpcheck"
	"github.com/Toshik1978/tgshd/pkg/health/check/network"
	hcfg "github.com/Toshik1978/tgshd/pkg/health/config"
	"github.com/Toshik1978/tgshd/pkg/health/runner"
	"github.com/Toshik1978/tgshd/pkg/health/store"
	"github.com/Toshik1978/tgshd/pkg/host/docker"
	"github.com/Toshik1978/tgshd/pkg/host/execprobe"
)

const (
	// Service check names; the http, freshness and command checks run one
	// check per entry, named by its condition key (<type>/<name>).
	containersCheckName = "containers"
	commandCheckName    = "command"
	// httpCheckPrefix and freshnessCheckPrefix start the names of the per-entry
	// http and freshness checks.
	httpCheckPrefix      = "http/"
	freshnessCheckPrefix = "freshness/"

	// kv keys of the checks' persisted state; a command entry's state is kept
	// under command.<name>.state.
	containersStateKey = "containers.state"
	commandStatePrefix = "command."
	commandStateSuffix = ".state"

	// The kv key of a freshness entry's first-missing mark is
	// freshness.<name>.first_missing (freshness.KVState).
	freshnessKeyPrefix = "freshness."
	freshnessKeySuffix = ".first_missing"
	// The stamp (Unix seconds) and read_ok keys feed the weekly summary.
	freshnessStampSuffix = ".stamp"

	// maxStampBytes bounds the read of a freshness timestamp file.
	maxStampBytes = 4096

	// Docker health states (container.HealthStatus).
	dockerHealthy  = "healthy"
	dockerNoHealth = "none"
)

// containersChecks provides the containers check when its section is present.
func containersChecks(hs *healthSetup, d *lazyDocker) checkSet {
	if hs.Config == nil || hs.Config.Containers == nil {
		return checkSet{}
	}

	return buildContainersCheck(hs.Config.Containers, d, time.Now)
}

// buildContainersCheck builds the containers check; a restarted container
// stays bad for the section's threshold. Its observations are persisted under
// containersStateKey so a daemon restart is not mistaken for a crash loop.
func buildContainersCheck(c *hcfg.Containers, src containers.Source, now func() time.Time) checkSet {
	chk, err := containers.New(src, c.Exclude, c.Threshold.Duration(), now)
	if err != nil {
		return checkSet{Errors: []startError{{Key: "config/containers/invalid", Text: err.Error()}}}
	}

	return checkSet{
		Checks:     []health.Check{newCheck(containersCheckName, c.Common, chk.Run)},
		Persisters: []runner.Persister{runner.KVPersister{Key: containersStateKey, State: bytesKV{state: chk}}},
	}
}

// httpChecks provides one http check per configured entry.
func httpChecks(hs *healthSetup, d *lazyDocker) checkSet {
	if hs.Config == nil || len(hs.Config.HTTP) == 0 {
		return checkSet{}
	}

	return buildHTTPChecks(hs.Config.HTTP, d)
}

// buildHTTPChecks builds one check per entry, named by its condition key, with
// the entry's own interval, threshold, mode, internet need and dependency. An
// entry that sets doh resolves through its own DoH resolver; the container
// requirement is judged by Docker. An entry that cannot be built is a start
// error and the others still run.
func buildHTTPChecks(entries []hcfg.HTTP, ctrs httpcheck.Containers) checkSet {
	var set checkSet
	for i := range entries {
		e := &entries[i]
		opts := httpcheck.Options{Containers: ctrs}
		if e.DoH != "" {
			// The default client has no timeout of its own; the check bounds
			// every lookup by the entry's timeout.
			opts.Resolver = doh.New(e.DoH, nil)
		}
		chk, err := httpcheck.New(*e, opts)
		if err != nil {
			set.Errors = append(set.Errors, startError{Key: "config/http/" + e.Name + "/invalid", Text: err.Error()})

			continue
		}
		c := newCheck(chk.Key(), e.Common, chk.Run)
		c.DependsOn = e.DependsOn
		set.Checks = append(set.Checks, c)
	}

	return set
}

// freshnessChecks provides one freshness check per configured entry. Their
// first-missing marks and stamps share one state, persisted in the store's
// kv; the weekly summary reads the stamps from it.
func freshnessChecks(hs *healthSetup) checkSet {
	if hs.Config == nil || len(hs.Config.Freshness) == 0 {
		return checkSet{}
	}
	names := make([]string, 0, len(hs.Config.Freshness))
	for i := range hs.Config.Freshness {
		names = append(names, hs.Config.Freshness[i].Name)
	}
	state := newFreshnessState(names)
	set := checkSet{Persisters: []runner.Persister{state}, Stamps: state}
	for _, e := range hs.Config.Freshness {
		chk := freshness.New(e, osFiles{}, state, time.Now)
		set.Checks = append(set.Checks, newCheck(freshnessCheckPrefix+e.Name, e.Common, chk.Run))
	}

	return set
}

// commandChecks provides one command check per configured entry. Under
// check --once (hs.OneShot) no fix is ever taken: the run reports what it
// would fix instead.
func commandChecks(hs *healthSetup, d *lazyDocker, logger *zap.Logger) checkSet {
	if hs.Config == nil || len(hs.Config.Command) == 0 {
		return checkSet{}
	}

	return buildCommandChecks(hs.Config.Command, execprobe.Runner{}, d, hs.OneShot, logger)
}

// buildCommandChecks builds one check per entry, named by its condition key.
// Each entry's strikes and fix history are persisted under
// command.<name>.state, and its fix records are handed to the runner. With
// dryRun the probe still runs, but fix commands and container restarts are
// only reported as what would have been done (command.WithDryRun).
func buildCommandChecks(
	entries []hcfg.Command, probes command.Runner, d command.Docker, dryRun bool, logger *zap.Logger,
) checkSet {
	var opts []command.Option
	if dryRun {
		opts = append(opts, command.WithDryRun())
	}
	var set checkSet
	for i := range entries {
		e := &entries[i]
		chk := command.New(*e, probes, d, time.Now, logger, opts...)
		set.Checks = append(set.Checks, newCheck(commandCheckName+"/"+e.Name, e.Common, chk.Run))
		set.Persisters = append(set.Persisters, runner.KVPersister{
			Key: commandStatePrefix + e.Name + commandStateSuffix, State: bytesKV{state: chk},
		})
		set.Fixes = append(set.Fixes, commandFixes{check: chk, mode: health.Mode(e.Mode)})
	}

	return set
}

// commandFixes hands a command check's fix records to the runner; a reported
// fix goes out on the entry's mode.
type commandFixes struct {
	check *command.Check
	mode  health.Mode
}

// TakeFixes implements runner.FixSource.
func (s commandFixes) TakeFixes() []runner.Fix {
	records := s.check.TakeFixes()
	out := make([]runner.Fix, 0, len(records))
	for _, r := range records {
		out = append(out, runner.Fix{
			At: r.At, Check: commandCheckName, Key: r.Key, Text: r.Text, Send: r.Send, Mode: s.mode,
		})
	}

	return out
}

// byteState is check state serialised as bytes (the containers and command
// checks).
type byteState interface {
	Export() ([]byte, error)
	Restore(data []byte) error
}

// bytesKV adapts a byteState to runner.KVState. Nothing stored (a fresh
// install) restores nothing.
type bytesKV struct {
	state byteState
}

// Export implements runner.KVState.
func (b bytesKV) Export() (string, error) {
	data, err := b.state.Export()
	if err != nil {
		return "", fmt.Errorf("export: %w", err)
	}

	return string(data), nil
}

// Restore implements runner.KVState.
func (b bytesKV) Restore(data string) error {
	if data == "" {
		return nil
	}
	if err := b.state.Restore([]byte(data)); err != nil {
		return fmt.Errorf("restore: %w", err)
	}

	return nil
}

// freshnessState is the shared freshness.State of the configured freshness
// entries, restored from and saved to the store's kv.
type freshnessState struct {
	names []string

	mu    sync.Mutex
	inner *freshness.KVState
}

// newFreshnessState returns an empty state for the named entries.
func newFreshnessState(names []string) *freshnessState {
	return &freshnessState{names: names, inner: freshness.NewKVState(nil)}
}

// FirstMissing implements freshness.State.
func (s *freshnessState) FirstMissing(name string) (time.Time, bool) {
	return s.state().FirstMissing(name)
}

// SetFirstMissing implements freshness.State.
func (s *freshnessState) SetFirstMissing(name string, t time.Time) {
	s.state().SetFirstMissing(name, t)
}

// RecordStamp implements freshness.State.
func (s *freshnessState) RecordStamp(name string, stamp time.Time) {
	s.state().RecordStamp(name, stamp)
}

// RecordNoStamp implements freshness.State.
func (s *freshnessState) RecordNoStamp(name string, readOK bool) {
	s.state().RecordNoStamp(name, readOK)
}

// Stamp returns the entry's last seen timestamp for the weekly summary
// (freshness.KVState.Stamp).
func (s *freshnessState) Stamp(name string) (stamp time.Time, present, readOK bool) {
	return s.state().Stamp(name)
}

// ClearFirstMissing implements freshness.State.
func (s *freshnessState) ClearFirstMissing(name string) {
	s.state().ClearFirstMissing(name)
}

// Restore implements runner.Persister.
func (s *freshnessState) Restore(st store.State) error {
	restored := freshness.NewKVState(st.KV)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.inner = restored

	return nil
}

// Save implements runner.Persister. The store's kv is upsert-only, so an
// entry without a mark or stamp is saved as an empty value (which reads as
// absent): a cleared mark would otherwise come back after a restart and skip
// the max_age grace of the next missing file, and a dropped stamp would show
// an old age.
func (s *freshnessState) Save(st *store.State) error {
	state := s.state()
	kv := state.Export()
	for _, name := range s.names {
		if _, ok := state.FirstMissing(name); !ok {
			kv[freshnessKey(name)] = ""
		}
		if _, ok := kv[freshnessKeyPrefix+name+freshnessStampSuffix]; !ok {
			kv[freshnessKeyPrefix+name+freshnessStampSuffix] = ""
		}
	}
	if len(kv) == 0 {
		return nil
	}
	if st.KV == nil {
		st.KV = make(map[string]string, len(kv))
	}
	maps.Copy(st.KV, kv)

	return nil
}

// state returns the current state under the lock.
func (s *freshnessState) state() *freshness.KVState {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.inner
}

// freshnessKey is the kv key of an entry's first-missing mark, as
// freshness.KVState documents it.
func freshnessKey(name string) string {
	return freshnessKeyPrefix + name + freshnessKeySuffix
}

// summaryGroups maps the service checks' names to their summary_group; a
// check without one is left out. The hardware checks are not listed: the
// summary's services block leaves them out, and the modem probe has no
// common keys.
func summaryGroups(c *hcfg.Config) map[string]string {
	groups := map[string]string{}
	if c == nil {
		return groups
	}
	add := func(name string, common hcfg.Common) {
		if common.SummaryGroup != "" {
			groups[name] = common.SummaryGroup
		}
	}
	if c.Network != nil {
		add(network.Name, c.Network.Common)
	}
	if c.UPS != nil {
		add(upsCheckName, c.UPS.Common)
	}
	if c.Containers != nil {
		add(containersCheckName, c.Containers.Common)
	}
	for i := range c.HTTP {
		add(httpCheckPrefix+c.HTTP[i].Name, c.HTTP[i].Common)
	}
	for i := range c.Freshness {
		add(freshnessCheckPrefix+c.Freshness[i].Name, c.Freshness[i].Common)
	}
	for i := range c.Command {
		add(commandCheckName+"/"+c.Command[i].Name, c.Command[i].Common)
	}

	return groups
}

// osFiles is the freshness check's file access.
type osFiles struct{}

// ModTime implements freshness.Source.
func (osFiles) ModTime(path string) (time.Time, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, fmt.Errorf("stat: %w", err)
	}

	return fi.ModTime(), nil
}

// ReadFile implements freshness.Source; a timestamp file is small, so a read
// is bounded by maxStampBytes.
func (osFiles) ReadFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxStampBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if len(data) > maxStampBytes {
		return nil, fmt.Errorf("larger than %d bytes", maxStampBytes)
	}

	return data, nil
}

// dockerAPI is what the service checks use of pkg/host/docker.
type dockerAPI interface {
	List(ctx context.Context) ([]docker.Container, error)
	Inspect(ctx context.Context, name string) (docker.Container, error)
	Restart(ctx context.Context, name string) error
	Close() error
}

// lazyDocker builds the Docker client on first use and retries a failed build
// on the next call, so a missing or broken Docker never stops the daemon from
// starting (FR-019): the Docker-backed checks fail their runs instead. Every
// Engine API call is bounded by the adapter's own timeouts.
type lazyDocker struct {
	newClient func() (dockerAPI, error)

	mu  sync.Mutex
	api dockerAPI
}

// newLazyDocker returns the lazy Docker client of pkg/host/docker, closed on
// stop when it was built.
func newLazyDocker(lc fx.Lifecycle) *lazyDocker {
	d := &lazyDocker{newClient: func() (dockerAPI, error) {
		c, err := docker.New()
		if err != nil {
			return nil, fmt.Errorf("docker: %w", err)
		}

		return c, nil
	}}
	lc.Append(fx.Hook{OnStop: func(context.Context) error { return d.Close() }})

	return d
}

// List implements containers.Source.
func (d *lazyDocker) List(ctx context.Context) ([]containers.Container, error) {
	api, err := d.client()
	if err != nil {
		return nil, err
	}
	in, err := api.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	out := make([]containers.Container, len(in))
	for i := range in {
		c := &in[i]
		out[i] = containers.Container{
			Name: c.Name, RestartPolicy: c.RestartPolicy, Status: c.Status, Health: c.Health,
			Running: c.Running, RestartCount: c.RestartCount, StartedAt: c.StartedAt,
		}
	}

	return out, nil
}

// ContainerHealthy implements httpcheck.Containers: the container runs and
// its health check, if it has one, reports healthy.
func (d *lazyDocker) ContainerHealthy(ctx context.Context, name string) (bool, error) {
	c, err := d.inspect(ctx, name)
	if err != nil {
		return false, err
	}
	switch c.Health {
	case "", dockerNoHealth, dockerHealthy:
		return c.Running, nil
	default:
		return false, nil
	}
}

// StartedAt implements command.Docker.
func (d *lazyDocker) StartedAt(ctx context.Context, name string) (time.Time, error) {
	c, err := d.inspect(ctx, name)
	if err != nil {
		return time.Time{}, err
	}

	return c.StartedAt, nil
}

// Restart implements command.Docker.
func (d *lazyDocker) Restart(ctx context.Context, name string) error {
	api, err := d.client()
	if err != nil {
		return err
	}
	if err := api.Restart(ctx, name); err != nil {
		return fmt.Errorf("docker: %w", err)
	}

	return nil
}

// Close closes the client when it was built.
func (d *lazyDocker) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.api == nil {
		return nil
	}
	err := d.api.Close()
	d.api = nil
	if err != nil {
		return fmt.Errorf("docker: %w", err)
	}

	return nil
}

// client returns the Docker client, building it when needed.
//
//nolint:ireturn // dockerAPI is the seam that lets tests fake the Engine API.
func (d *lazyDocker) client() (dockerAPI, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.api == nil {
		api, err := d.newClient()
		if err != nil {
			return nil, err
		}
		d.api = api
	}

	return d.api, nil
}

// inspect inspects the named container.
func (d *lazyDocker) inspect(ctx context.Context, name string) (docker.Container, error) {
	api, err := d.client()
	if err != nil {
		return docker.Container{}, err
	}
	c, err := api.Inspect(ctx, name)
	if err != nil {
		return docker.Container{}, fmt.Errorf("docker: %w", err)
	}

	return c, nil
}
