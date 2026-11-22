package fingerprint

// TrackView is one track's windowed view for cross-track comparison.
// Tracks must share the same window size.
type TrackView struct {
	Name    string
	Hashes  []uint64
	Samples []int16
	Window  int
	Silent  func(windowIndex int) bool // optional silence map
}

// FindCrossDuplicates scans tracks in order (earliest first) and
// reports every window whose content already appeared in an earlier
// track. Hash candidates are verified against the raw samples, so
// collisions are rejected; silent windows never participate.
func FindCrossDuplicates(tracks []TrackView) []Duplicate {
	type ref struct {
		track  int
		window int
	}
	seen := map[uint64][]ref{}
	var out []Duplicate
	for ti, tv := range tracks {
		for i, h := range tv.Hashes {
			if tv.Silent != nil && tv.Silent(i) {
				continue
			}
			matched := false
			for _, r := range seen[h] {
				a := windowSlice(tracks[r.track].Samples, r.window, tv.Window)
				b := windowSlice(tv.Samples, i, tv.Window)
				if a != nil && b != nil && equalSamples(a, b) {
					out = append(out, Duplicate{
						WindowIndex: i,
						Earliest:    tracks[r.track].Name,
						RepeatedBy:  tv.Name,
					})
					matched = true
					break
				}
			}
			if !matched {
				seen[h] = append(seen[h], ref{track: ti, window: i})
			}
		}
	}
	return out
}
