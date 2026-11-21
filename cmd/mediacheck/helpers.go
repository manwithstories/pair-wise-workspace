package main

import (
	"fmt"
	"sort"
	"strings"

	"mediacheck/internal/clock"
	"mediacheck/internal/fingerprint"
	"mediacheck/internal/split"
	"mediacheck/internal/txn"
)

// fragView is the per-fragment view the fingerprint stage needs.
type fragView struct {
	startMs float64
	endMs   float64
	payload []byte
	samples int
	silent  bool
}

// buildFingerprintSegments assembles content-addressable segments from
// the cut ranges and the fragment payloads overlapping them.
func buildFingerprintSegments(perTrack map[string][]split.Segment, trackFrags map[string][]clock.Frag, parsed []parsedFrag, silentFrag map[string]bool) []fingerprint.Segment {
	// Index parsed payloads by track/seq.
	type key struct {
		track string
		seq   uint32
	}
	payloads := map[key]*parsedFrag{}
	for i := range parsed {
		payloads[key{parsed[i].track, parsed[i].info.Seq}] = &parsed[i]
	}
	var out []fingerprint.Segment
	for track, segs := range perTrack {
		// Build the timed payload list for this track.
		var views []fragView
		for _, f := range trackFrags[track] {
			pf, ok := payloads[key{track, f.Seq}]
			if !ok {
				continue
			}
			views = append(views, fragView{
				startMs: clock.ToMs(f.TFDT, f.Timescale),
				endMs:   clock.ToMs(f.TFDT+f.Duration, f.Timescale),
				payload: pf.info.Payload,
				samples: len(pf.info.Samples),
				silent:  silentFrag[fmt.Sprintf("%s/%d", track, f.Seq)],
			})
		}
		sort.Slice(views, func(i, j int) bool { return views[i].startMs < views[j].startMs })
		for _, seg := range segs {
			fs := fingerprint.Segment{
				ID:      fmt.Sprintf("%s#%03d", track, seg.Index),
				Track:   track,
				StartMs: seg.ActStartMs,
				EndMs:   seg.ActEndMs,
			}
			allSilent := true
			for _, v := range views {
				if v.endMs <= seg.ActStartMs || v.startMs >= seg.ActEndMs {
					continue
				}
				fs.Payload = append(fs.Payload, v.payload...)
				fs.Samples += v.samples
				if !v.silent {
					allSilent = false
				}
			}
			fs.Silent = allSilent && fs.Samples > 0
			out = append(out, fs)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// buildOutputs renders the committed artifact set: one manifest per
// segment plus a duplicate report. Content is deterministic (no wall
// clock, no map iteration order).
func buildOutputs(res *result, dups []fingerprint.Duplicate) []txn.Output {
	var outs []txn.Output
	segs := make([]split.Segment, len(res.segments))
	copy(segs, res.segments)
	sort.Slice(segs, func(i, j int) bool {
		if segs[i].Track != segs[j].Track {
			return segs[i].Track < segs[j].Track
		}
		return segs[i].Index < segs[j].Index
	})
	for _, s := range segs {
		content := fmt.Sprintf(
			"track=%s index=%d req=[%.1f,%.1f) actual=[%.3f,%.3f) units=[%d,%d)\n",
			s.Track, s.Index, s.ReqStartMs, s.ReqEndMs, s.ActStartMs, s.ActEndMs,
			s.StartUnits, s.EndUnits)
		outs = append(outs, txn.Output{
			Name:    fmt.Sprintf("segment-%s-%03d.txt", s.Track, s.Index),
			Content: []byte(content),
		})
	}
	var b strings.Builder
	for _, d := range dups {
		fmt.Fprintf(&b, "duplicate hash=%s track=%s segments=%s ranges=",
			d.Hash, d.Track, strings.Join(d.SegmentIDs, ","))
		for _, r := range d.TimeRanges {
			fmt.Fprintf(&b, " [%.1f,%.1f)", r[0], r[1])
		}
		b.WriteString("\n")
	}
	outs = append(outs, txn.Output{Name: "duplicates.txt", Content: []byte(b.String())})
	return outs
}
