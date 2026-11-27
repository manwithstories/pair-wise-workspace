package dag

import (
	"errors"
	"fmt"
	"time"
)

// Node is one unit of relay work. Digests/Parameters feed the idempotency
// key; Run executes the work and returns the artifact reference to journal.
type Node interface {
	Name() string
	DependsOn() []string
	InputDigests() map[string]string
	Parameters() map[string]string
	Run(ctx *Context) (output string, err error)
}

// Context gives a node access to already-succeeded dependency outputs.
type Context struct {
	Outputs map[string]string
}

// BlockedError names the first dependency that prevented a node from running.
type BlockedError struct {
	Node string
	By   string
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("dag: node %q blocked by failed dependency %q", e.Node, e.By)
}

// PauseError marks a deliberate pause point; exit handling maps it to 75.
type PauseError struct{ Node string }

func (e *PauseError) Error() string {
	return fmt.Sprintf("dag: paused after node %q", e.Node)
}

// Runner executes a node set honoring dependencies and journal state.
type Runner struct {
	journal    *Journal
	pauseAfter string
	clock      func() time.Time
}

// NewRunner wires a runner to its journal. pauseAfter names a node after
// which execution should stop deliberately (empty disables pausing).
func NewRunner(j *Journal, pauseAfter string) *Runner {
	return &Runner{journal: j, pauseAfter: pauseAfter, clock: time.Now}
}

// RunResult summarizes one invocation for deterministic terminal output.
type RunResult struct {
	Executed   []string
	Skipped    []string // already-succeeded nodes reused on resume
	Blocked    []string
	FirstBlock *BlockedError
	// Failures maps a node that itself returned an error to that error text.
	Failures map[string]string
	PausedAt string
}

// Run topologically executes nodes. Succeeded nodes are skipped (their
// journaled output is reused); a failed dependency blocks dependents and the
// first blocking relationship is reported rather than crashing the graph.
func (r *Runner) Run(nodes []Node) (RunResult, error) {
	byName := map[string]Node{}
	indeg := map[string]int{}
	for _, n := range nodes {
		byName[n.Name()] = n
		indeg[n.Name()] = len(n.DependsOn())
	}
	if cycle := findCycle(nodes); cycle != "" {
		return RunResult{}, fmt.Errorf("dag: dependency cycle at %q", cycle)
	}

	res := RunResult{Failures: map[string]string{}}
	outputs := map[string]string{}
	done := map[string]bool{}
	failed := map[string]string{}

	// Kahn's algorithm; process in declaration order among ready nodes.
	for len(done)+len(failed) < len(nodes) {
		progressed := false
		for _, n := range nodes {
			name := n.Name()
			if done[name] || failed[name] != "" {
				continue
			}
			if block := r.firstBlocker(n, done, failed); block != "" {
				failed[name] = block
				be := &BlockedError{Node: name, By: block}
				res.Blocked = append(res.Blocked, name)
				if res.FirstBlock == nil {
					res.FirstBlock = be
				}
				progressed = true
				continue
			}
			if !r.depsReady(n, done) {
				continue
			}
			if ok, failText := r.runOne(n, outputs, &res); ok {
				done[name] = true
			} else {
				failed[name] = name
				res.Failures[name] = failText
			}
			progressed = true
			if name == r.pauseAfter {
				res.PausedAt = name
				return res, &PauseError{Node: name}
			}
		}
		if !progressed {
			return res, errors.New("dag: unable to make progress")
		}
	}
	return res, nil
}

func (r *Runner) runOne(n Node, outputs map[string]string, res *RunResult) (bool, string) {
	key := IdempotencyKey(n.Name(), n.InputDigests(), n.Parameters())
	if r.journal.IsDone(key) {
		prev, _ := r.journal.Get(key)
		outputs[n.Name()] = prev.Output
		res.Skipped = append(res.Skipped, n.Name())
		return true, ""
	}
	_ = r.journal.Put(Entry{Key: key, Name: n.Name(), Status: StatusRunning,
		UpdatedAt: r.clock().UTC().Format(time.RFC3339Nano)})
	out, err := n.Run(&Context{Outputs: outputs})
	entry := Entry{Key: key, Name: n.Name(), UpdatedAt: r.clock().UTC().Format(time.RFC3339Nano), Output: out}
	if err != nil {
		entry.Status = StatusFailed
		entry.Error = err.Error()
		_ = r.journal.Put(entry)
		return false, err.Error()
	}
	entry.Status = StatusSucceeded
	_ = r.journal.Put(entry)
	outputs[n.Name()] = out
	res.Executed = append(res.Executed, n.Name())
	return true, ""
}

func (r *Runner) depsReady(n Node, done map[string]bool) bool {
	for _, d := range n.DependsOn() {
		if !done[d] {
			return false
		}
	}
	return true
}

// firstBlocker returns a dependency name that already failed, or "".
func (r *Runner) firstBlocker(n Node, done map[string]bool, failed map[string]string) string {
	for _, d := range n.DependsOn() {
		if _, bad := failed[d]; bad {
			return d
		}
	}
	return ""
}

// findCycle returns a node participating in a cycle, or "" when acyclic.
func findCycle(nodes []Node) string {
	color := map[string]int{} // 0 white, 1 gray, 2 black
	adj := map[string][]string{}
	for _, n := range nodes {
		adj[n.Name()] = n.DependsOn()
	}
	var visit func(string) string
	visit = func(name string) string {
		color[name] = 1
		for _, d := range adj[name] {
			if color[d] == 1 {
				return d
			}
			if color[d] == 0 {
				if c := visit(d); c != "" {
					return c
				}
			}
		}
		color[name] = 2
		return ""
	}
	for _, n := range nodes {
		if color[n.Name()] == 0 {
			if c := visit(n.Name()); c != "" {
				return c
			}
		}
	}
	return ""
}
