package dag

import (
	"context"
	"fmt"
	"sort"

	"mediaqc/internal/media"
)

// RunFunc executes a deterministic node using dependency output bytes.
type RunFunc func(ctx context.Context, deps map[string][]byte) ([]byte, error)

// Node is one stable-content-addressed DAG vertex.
type Node struct {
	ID          string
	Deps        []string
	Input       []byte
	Run         RunFunc
	MaxAttempts int
}

// Event records a scheduling/retry transition for terminal output.
type Event struct {
	NodeID     string   `json:"node_id"`
	Attempt    int      `json:"attempt"`
	Kind       string   `json:"kind"`
	InputHash  string   `json:"input_hash,omitempty"`
	OutputHash string   `json:"output_hash,omitempty"`
	ErrorType  string   `json:"error_type,omitempty"`
	Message    string   `json:"message,omitempty"`
	Blocked    []string `json:"blocked,omitempty"`
}

// Report is the deterministic scheduling result.
type Report struct {
	Order        []string          `json:"order"`
	Executed     []string          `json:"executed"`
	Cached       []string          `json:"cached"`
	Retried      []string          `json:"retried"`
	Events       []Event           `json:"events"`
	FirstBlocked string            `json:"first_blocked"`
	Blocked      []string          `json:"blocked_nodes,omitempty"`
	Outputs      map[string][]byte `json:"-"`
}

// Scheduler runs dependencies in stable topological order with a Journal.
type Scheduler struct {
	nodes           map[string]*Node
	Journal         *Journal
	DefaultAttempts int
}

// NewScheduler creates a scheduler backed by j.
func NewScheduler(j *Journal) *Scheduler {
	if j == nil {
		j = NewJournal()
	}
	return &Scheduler{nodes: make(map[string]*Node), Journal: j, DefaultAttempts: 2}
}

// Add registers a node.
func (s *Scheduler) Add(n *Node) error {
	if n == nil || n.ID == "" || n.Run == nil {
		return media.New(media.KindInternal, "dag.Add", "invalid node")
	}
	if _, exists := s.nodes[n.ID]; exists {
		return media.New(media.KindInternal, "dag.Add", "duplicate node "+n.ID)
	}
	for _, dep := range n.Deps {
		if dep == n.ID {
			return media.New(media.KindDependency, "dag.Add", "self dependency "+n.ID)
		}
	}
	s.nodes[n.ID] = n
	return nil
}

// Run executes the graph once, retrying retryable node failures.
func (s *Scheduler) Run(ctx context.Context) (*Report, error) {
	order, err := s.topologicalOrder()
	if err != nil {
		return nil, err
	}
	rep := &Report{Order: order, Outputs: make(map[string][]byte)}
	depHashes := make(map[string]string)

	for _, id := range order {
		if err := ctx.Err(); err != nil {
			return rep, media.Wrap(media.KindInternal, "dag.Run", "canceled", err)
		}
		node := s.nodes[id]
		deps := make(map[string][]byte, len(node.Deps))
		for _, dep := range node.Deps {
			deps[dep] = rep.Outputs[dep]
		}
		hash := InputHash(id, node.Input, depHashes)

		if rec, ok := s.Journal.Get(id); ok && rec.Status == "success" && rec.InputHash == hash {
			rep.Outputs[id] = rec.Output
			depHashes[id] = rec.OutputHash
			rep.Cached = append(rep.Cached, id)
			rep.Events = append(rep.Events, Event{NodeID: id, Kind: "cache_hit", InputHash: hash, OutputHash: rec.OutputHash})
			continue
		}

		attempts := node.MaxAttempts
		if attempts <= 0 {
			attempts = s.DefaultAttempts
		}
		var lastErr error
		for attempt := 1; attempt <= attempts; attempt++ {
			if attempt > 1 {
				rep.Events = append(rep.Events, Event{NodeID: id, Attempt: attempt, Kind: "retry_start", InputHash: hash})
			}
			out, runErr := node.Run(ctx, deps)
			if runErr == nil {
				rec := s.Journal.RecordSuccess(id, hash, out, attempt)
				rep.Outputs[id] = out
				depHashes[id] = rec.OutputHash
				if attempt > 1 {
					rep.Retried = append(rep.Retried, id)
					rep.Events = append(rep.Events, Event{NodeID: id, Attempt: attempt, Kind: "retry_success", InputHash: hash, OutputHash: rec.OutputHash})
				} else {
					rep.Executed = append(rep.Executed, id)
					rep.Events = append(rep.Events, Event{NodeID: id, Attempt: attempt, Kind: "success", InputHash: hash, OutputHash: rec.OutputHash})
				}
				lastErr = nil
				break
			}

			lastErr = runErr
			kind := media.Classify(runErr)
			blocked := s.transitiveDependents(id)
			rep.FirstBlocked = id
			rep.Blocked = blocked
			rep.Events = append(rep.Events, Event{
				NodeID: id, Attempt: attempt, Kind: "attempt_failed", InputHash: hash,
				ErrorType: string(kind), Message: runErr.Error(), Blocked: blocked,
			})
			if len(blocked) > 0 {
				rep.Events = append(rep.Events, Event{
					NodeID: id, Attempt: attempt, Kind: "blocked_dependents",
					Message: fmt.Sprintf("dependency %s blocks %v", id, blocked), Blocked: blocked,
				})
			}
			if attempt == attempts || !retryable(kind) {
				return rep, runErr
			}
		}
		if lastErr != nil {
			return rep, lastErr
		}
	}
	return rep, nil
}

func retryable(k media.Kind) bool {
	return k == media.KindInternal || k == media.KindTimeout
}

func (s *Scheduler) topologicalOrder() ([]string, error) {
	ids := make([]string, 0, len(s.nodes))
	for id := range s.nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	done := make(map[string]bool, len(ids))
	order := make([]string, 0, len(ids))
	for len(order) < len(ids) {
		progress := false
		for _, id := range ids {
			if done[id] {
				continue
			}
			ready := true
			for _, dep := range s.nodes[id].Deps {
				if _, ok := s.nodes[dep]; !ok {
					return nil, media.New(media.KindDependency, "dag.topologicalOrder", id+" references missing dependency "+dep)
				}
				if !done[dep] {
					ready = false
					break
				}
			}
			if ready {
				order = append(order, id)
				done[id] = true
				progress = true
				break // restart from sorted first eligible node for stable order
			}
		}
		if !progress {
			return nil, media.New(media.KindDependency, "dag.topologicalOrder", "dependency cycle detected")
		}
	}
	return order, nil
}

func (s *Scheduler) transitiveDependents(root string) []string {
	reverse := make(map[string][]string)
	for id, n := range s.nodes {
		for _, dep := range n.Deps {
			reverse[dep] = append(reverse[dep], id)
		}
	}
	seen := make(map[string]bool)
	queue := append([]string(nil), reverse[root]...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] || id == root {
			continue
		}
		seen[id] = true
		queue = append(queue, reverse[id]...)
	}
	out := make([]string, 0, len(seen))
	if order, err := s.topologicalOrder(); err == nil {
		for _, id := range order {
			if seen[id] {
				out = append(out, id)
			}
		}
	} else {
		for id := range seen {
			out = append(out, id)
		}
		sort.Strings(out)
	}
	return out
}
