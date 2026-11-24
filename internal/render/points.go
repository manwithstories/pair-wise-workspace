package render

import (
	"mediarun/internal/clock"
	"mediarun/internal/model"
)

type point struct {
	start clock.Tick
	track uint32
	kind  model.Kind
}

// reviewPoints selects distinct ticks in ascending order, preferring frame
// events as the anchor so a reviewer sees picture timing first.
func reviewPoints(events []model.Event) []point {
	seen := map[clock.Tick]bool{}
	var out []point
	for _, ev := range events {
		if ev.Kind != model.KindFrame {
			continue
		}
		if seen[ev.Start] {
			continue
		}
		seen[ev.Start] = true
		out = append(out, point{start: ev.Start, track: ev.TrackID, kind: ev.Kind})
	}
	for _, ev := range events {
		if ev.Kind == model.KindFrame || seen[ev.Start] {
			continue
		}
		seen[ev.Start] = true
		out = append(out, point{start: ev.Start, track: ev.TrackID, kind: ev.Kind})
	}
	sortPoints(out)
	return out
}

func sortPoints(ps []point) {
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0 && ps[j].start < ps[j-1].start; j-- {
			ps[j], ps[j-1] = ps[j-1], ps[j]
		}
	}
}
