package runner

import (
	"fmt"

	"github.com/Toshik1978/tgshd/pkg/health/notify"
	"github.com/Toshik1978/tgshd/pkg/health/store"
)

const (
	// KeyRestore is the checker error of a persister that cannot restore.
	KeyRestore = "health/state/restore"
	keyPersist = "health/state/save"
)

// Persister keeps a check's own state (e.g. drive baselines, a log cursor) in
// the store. Restore receives the stored state once, after the first
// successful Load (an empty state when the runner has no store). Checks keep
// running from memory while Load fails, so they may run before Restore: a
// check whose judgement depends on its restored state must refuse to run
// until then (as the disks and kernel checks do). Save adds the check's state
// to every checkpoint, which only happens once loaded. A failure of either is
// a checker error; the others still run.
type Persister interface {
	Restore(st store.State) error
	Save(st *store.State) error
}

// KVState is check state kept as one kv value; the kernel and temperatures
// checks satisfy it.
type KVState interface {
	Export() (string, error)
	Restore(data string) error
}

// KVPersister persists a KVState under Key in the store's kv.
type KVPersister struct {
	Key   string
	State KVState
}

// Restore implements Persister: it hands over the stored value, empty when
// there is none.
func (p KVPersister) Restore(st store.State) error {
	if err := p.State.Restore(st.KV[p.Key]); err != nil {
		return fmt.Errorf("restore %s: %w", p.Key, err)
	}

	return nil
}

// Save implements Persister. A failed export leaves the kv untouched.
func (p KVPersister) Save(st *store.State) error {
	data, err := p.State.Export()
	if err != nil {
		return fmt.Errorf("save %s: %w", p.Key, err)
	}
	if st.KV == nil {
		st.KV = map[string]string{}
	}
	st.KV[p.Key] = data

	return nil
}

// kvFuncs is a KVState made of an export and a restore function.
type kvFuncs struct {
	export  func() (string, error)
	restore func(data string) error
}

// Export implements KVState.
func (f kvFuncs) Export() (string, error) { return f.export() }

// Restore implements KVState.
func (f kvFuncs) Restore(data string) error { return f.restore(data) }

// deliveryPersisters keep the notifier's delivery state in kv, so a restart
// neither emails the Telegram failure twice (FR-029), nor sends a PROBLEM by
// SMS again or loses the SMS of its RESOLVED (FR-030), nor loses a listed
// PROBLEM or the Folded count (FR-031).
func deliveryPersisters(n *notify.Notifier) []Persister {
	return []Persister{
		KVPersister{Key: notify.KVChannels, State: kvFuncs{export: n.ExportChannels, restore: n.RestoreChannels}},
		KVPersister{
			Key:   notify.KVSMSProblems,
			State: kvFuncs{export: n.ExportSMSProblems, restore: n.RestoreSMSProblems},
		},
		KVPersister{
			Key:   notify.KVUndelivered,
			State: kvFuncs{export: n.ExportUndelivered, restore: n.RestoreUndelivered},
		},
	}
}

// ownPersisters keep the runner's own state in kv: the notifier's delivery
// state, then the maintenance-lock observation, so the stuck limit (FR-027)
// keeps its clock across a restart.
func ownPersisters(r *Runner) []Persister {
	return append(deliveryPersisters(r.notifier), KVPersister{
		Key:   KVMaintenance,
		State: kvFuncs{export: r.exportMaintenance, restore: r.restoreMaintenance},
	})
}

// restore hands the loaded state to every persister in ps and keeps the
// failures for raiseLoadFailure. The caller holds r.loadMu.
func (r *Runner) restore(ps []Persister, st store.State) {
	for _, p := range ps {
		if err := p.Restore(st); err != nil {
			r.restoreErrs = append(r.restoreErrs, err)
		}
	}
}

// persist adds the runner's own state and every persister's state to a
// checkpoint, raising failures.
func (r *Runner) persist(st *store.State) {
	r.save(r.own, st)
	r.save(r.opts.Persisters, st)
}

// save adds the state of every persister in ps to a checkpoint, raising
// failures.
func (r *Runner) save(ps []Persister, st *store.State) {
	for _, p := range ps {
		if err := p.Save(st); err != nil {
			r.CheckerError(keyPersist, "cannot save state: "+err.Error())
		}
	}
}
