package graph

// Step is one entry of the deterministic processing plan.
type Step struct {
	Node     NodeID   `json:"node"`
	Kind     NodeKind `json:"kind"`
	Track    string   `json:"track"`
	Segment  string   `json:"segment"`
	Start    int64    `json:"start_tick"`
	Duration int64    `json:"duration_tick"`
	Wave     int      `json:"wave"`
}

// Plan is the ordered result of a topological sort. Steps with equal wake
// time keep a stable order (track, start, node id), so identical input and
// parameters always produce an identical plan.
type Plan struct {
	Steps []Step `json:"steps"`
	Waves int    `json:"waves"`
}

// BuildPlan validates, builds and orders the graph.
func BuildPlan(t *Timeline) (*Plan, error) {
	g, err := Build(t)
	if err != nil {
		return nil, err
	}
	order, err := g.topoOrder()
	if err != nil {
		return nil, err
	}
	return g.planFromOrder(order), nil
}

func (g *Graph) planFromOrder(order []NodeID) *Plan {
	p := &Plan{Steps: make([]Step, 0, len(order))}
	waveOf := make(map[NodeID]int, len(order))
	maxWave := 0
	for _, id := range order {
		n, _ := g.Node(id)
		wave := 0
		for _, d := range n.Deps {
			if w := waveOf[d] + 1; w > wave {
				wave = w
			}
		}
		waveOf[id] = wave
		if wave > maxWave {
			maxWave = wave
		}
		p.Steps = append(p.Steps, Step{
			Node: n.ID, Kind: n.Kind, Track: n.Track, Segment: n.Segment,
			Start: n.Start, Duration: n.Duration, Wave: wave,
		})
	}
	p.Waves = maxWave + 1
	return p
}
