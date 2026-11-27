package pipeline

import (
	"sort"

	"avrepair/internal/journal"
	"avrepair/internal/media"
)

func (c *runContext) hashTimeline(tl *media.Timeline) string {
	payload := struct {
		Base        media.Rational     `json:"base"`
		Tracks      []*media.Track     `json:"tracks"`
		Corrections []media.Correction `json:"corrections"`
		Quarantine  []media.Quarantine `json:"quarantine"`
	}{tl.Base, tl.Tracks, tl.Corrections, tl.Quarantine}
	data, err := jsonEncode(payload)
	if err != nil {
		return ""
	}
	return sha256Hex(data)
}

func avSyncMax(tl *media.Timeline) media.Rational {
	var v, a *media.Track
	for _, t := range tl.Tracks {
		if v == nil && t.Kind == media.TrackVideo {
			v = t
		}
		if a == nil && t.Kind == media.TrackAudio {
			a = t
		}
	}
	if v == nil || a == nil {
		return media.Zero
	}
	var diffs []media.Rational
	for _, vs := range v.Segments {
		as := findSegmentBySeq(a, vs.Seq)
		if as == nil || len(as.Samples) == 0 || len(vs.Samples) == 0 || !vs.Valid() {
			continue
		}
		diffs = append(diffs, as.Samples[0].PTS.Sub(vs.Samples[0].PTS).Abs())
	}
	if len(diffs) == 0 {
		return media.Zero
	}
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].Cmp(diffs[j]) < 0 })
	return diffs[len(diffs)-1]
}

func countSegments(tl *media.Timeline, keep func(*media.Segment) bool) int {
	n := 0
	for _, t := range tl.Tracks {
		for _, s := range t.Segments {
			if keep == nil || keep(s) {
				n++
			}
		}
	}
	return n
}

func sanitize(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			out = append(out, r)
		}
	}
	if out == nil {
		return "seg"
	}
	return string(out)
}

func atomicWrite(path string, data []byte) error {
	return journal.AtomicWrite(path, data, 0o644)
}
