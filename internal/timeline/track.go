package timeline

import "fmt"

// Track describes one media track in the recovery graph.
type Track struct {
	SSRC      uint32
	ClockRate uint32
	Kind      string // "video" or "audio"
	DependsOn []uint32
	TrackID   uint32 // 1-based MP4 track ID, assigned in topo order
}

// Graph is the track dependency DAG used to order repair and indexing.
type Graph struct {
	bySSRC map[uint32]*Track
	order  []*Track
}

func NewGraph() *Graph { return &Graph{bySSRC: map[uint32]*Track{}} }

// Add registers a track; duplicate SSRCs are rejected.
func (g *Graph) Add(t *Track) error {
	if _, ok := g.bySSRC[t.SSRC]; ok {
		return fmt.Errorf("duplicate track SSRC %d", t.SSRC)
	}
	g.bySSRC[t.SSRC] = t
	g.order = append(g.order, t)
	return nil
}

func (g *Graph) Get(ssrc uint32) *Track { return g.bySSRC[ssrc] }
func (g *Graph) Len() int               { return len(g.order) }

// TopoOrder returns tracks in dependency order (dependencies first)
// and assigns MP4 track IDs. Cycles and missing dependencies are
// rejected as media-boundary errors.
func (g *Graph) TopoOrder() ([]*Track, error) {
	const (
		white = 0 // unvisited
		gray  = 1 // on stack
		black = 2 // done
	)
	color := map[uint32]int{}
	var out []*Track
	var visit func(t *Track) error
	visit = func(t *Track) error {
		switch color[t.SSRC] {
		case gray:
			return fmt.Errorf("dependency cycle at track SSRC %d", t.SSRC)
		case black:
			return nil
		}
		color[t.SSRC] = gray
		for _, dep := range t.DependsOn {
			d, ok := g.bySSRC[dep]
			if !ok {
				return fmt.Errorf("track SSRC %d depends on missing SSRC %d", t.SSRC, dep)
			}
			if err := visit(d); err != nil {
				return err
			}
		}
		color[t.SSRC] = black
		out = append(out, t)
		return nil
	}
	for _, t := range g.order {
		if err := visit(t); err != nil {
			return nil, err
		}
	}
	for i, t := range out {
		t.TrackID = uint32(i + 1)
	}
	return out, nil
}
