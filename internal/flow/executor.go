package flow

import (
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/pairwise/mediad/internal/media"
	"github.com/pairwise/mediad/internal/metric"
	"github.com/pairwise/mediad/internal/store"
)

// Executor persists artifacts and DAG state through LocalStore.
type Executor struct {
	Store       *store.LocalStore
	StateRel    string
	MaxParallel int
	state       persistedState
	Executions  int
	Writes      int
	Cached      int
}

type persistedState struct {
	Nodes map[string]*Node `json:"nodes"`
}

// NewExecutor loads any prior state for resume.
func NewExecutor(st *store.LocalStore, stateRel string) (*Executor, error) {
	if stateRel == "" {
		stateRel = "dag-state.json"
	}
	e := &Executor{
		Store:       st,
		StateRel:    stateRel,
		MaxParallel: store.MaxTasks,
		state:       persistedState{Nodes: map[string]*Node{}},
	}
	if st.Exists(stateRel) {
		data, err := st.Read(stateRel)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &e.state); err != nil {
			return nil, media.Wrap(err, media.CodeMalformed, "flow.dag", stateRel, "invalid persisted DAG state")
		}
	}
	return e, nil
}

func (e *Executor) mergePersisted(n *Node) {
	old := e.state.Nodes[n.ID]
	if old == nil || old.InputHash != n.InputHash {
		return
	}
	n.State = old.State
	n.Attempts = old.Attempts
	n.SHA256 = old.SHA256
	n.Detail = old.Detail
	n.BlockedBy = old.BlockedBy
}

func (e *Executor) persist(d *DAG) error {
	e.state.Nodes = make(map[string]*Node, len(d.nodes))
	for id, n := range d.nodes {
		cp := *n
		cp.Produce = nil
		e.state.Nodes[id] = &cp
	}
	data, err := json.MarshalIndent(e.state, "", "  ")
	if err != nil {
		return media.Wrap(err, media.CodeInternal, "flow.dag", "state", "marshal failed")
	}
	return e.Store.WriteAtomic(e.StateRel, data)
}

// Run executes unfinished nodes in topological order. Completed matching nodes
// are skipped. Independent nodes continue after one blocked/failed node.
func (e *Executor) Run(d *DAG) error {
	order, err := d.TopologicalOrder()
	if err != nil {
		return err
	}
	sem := store.NewSemaphore(e.MaxParallel)
	var firstErr error
	for _, id := range order {
		n := d.nodes[id]
		e.mergePersisted(n)
		if n.State == StateDone && n.SHA256 != "" && (n.OutputRel == "" || e.Store.Exists(n.OutputRel)) {
			e.Cached++
			continue
		}
		if blocker := e.dependencyBlocked(d, n); blocker != "" {
			n.State = StateBlocked
			n.BlockedBy = blocker
			n.Detail = "blocked by dependency " + blocker
			if firstErr == nil {
				firstErr = media.NewError(media.CodeBlockedDependency, "flow.dag", blocker, n.ID+" blocked")
			}
			_ = e.persist(d)
			continue
		}
		if n.Produce == nil {
			n.State = StateFailed
			n.Detail = "missing produce function"
			if firstErr == nil {
				firstErr = media.NewError(media.CodeInternal, "flow.dag", n.ID, n.Detail)
			}
			continue
		}
		err := sem.Run(func() error { return e.runNode(n) })
		if err != nil {
			var me *media.Error
			if errors.As(err, &me) && me.Code == media.CodeBlockedDependency {
				n.State = StateBlocked
				n.BlockedBy = n.ID
			} else {
				n.State = StateFailed
			}
			n.Detail = err.Error()
			if firstErr == nil {
				firstErr = err
			}
		} else {
			n.State = StateDone
			n.Detail = ""
		}
		if perr := e.persist(d); perr != nil && firstErr == nil {
			firstErr = perr
		}
	}
	return firstErr
}

func (e *Executor) dependencyBlocked(d *DAG, n *Node) string {
	for _, depID := range n.Deps {
		dep := d.nodes[depID]
		if dep.State != StateDone {
			if dep.BlockedBy != "" {
				return dep.BlockedBy
			}
			return depID
		}
	}
	return ""
}

func (e *Executor) runNode(n *Node) error {
	e.Executions++
	var data []byte
	var err error
	for attempt := 0; attempt <= n.Retries; attempt++ {
		n.Attempts++
		data, err = n.Produce()
		if err == nil {
			break
		}
		var me *media.Error
		if errors.As(err, &me) && me.Code == media.CodeBlockedDependency {
			return err
		}
		if attempt == n.Retries {
			return err
		}
	}
	if n.OutputRel != "" {
		if err := e.Store.WriteAtomic(n.OutputRel, data); err != nil {
			return err
		}
		e.Writes++
		hash, err := metric.FileSHA256(filepath.Join(e.Store.Root, n.OutputRel))
		if err != nil {
			return media.Wrap(err, media.CodeLocalIO, "flow.dag", n.OutputRel, "hash after atomic write failed")
		}
		n.SHA256 = hash
	} else {
		n.SHA256 = metric.BytesSHA256(data)
	}
	return nil
}
