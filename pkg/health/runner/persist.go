package runner

import (
	"fmt"

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

// restore hands the loaded state to every persister and keeps the failures
// for raiseLoadFailure. The caller holds r.loadMu.
func (r *Runner) restore(st store.State) {
	for _, p := range r.opts.Persisters {
		if err := p.Restore(st); err != nil {
			r.restoreErrs = append(r.restoreErrs, err)
		}
	}
}

// persist adds every persister's state to a checkpoint, raising failures.
func (r *Runner) persist(st *store.State) {
	for _, p := range r.opts.Persisters {
		if err := p.Save(st); err != nil {
			r.CheckerError(keyPersist, "cannot save state: "+err.Error())
		}
	}
}
