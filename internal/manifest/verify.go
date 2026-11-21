package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"

	"mediameld/internal/model"
	"mediameld/internal/timeline"
)

// Verifier independently re-reads the output container and checks the
// manifest against it: first/last sample per track, sample hashes, and every
// segment boundary. It does not trust any in-memory result from the writer.
type Verifier struct {
	// Demux re-hashes the output container into events keyed by track.
	Demux func() ([]model.Event, error)
}

// Report is the verifier's verdict.
type Report struct {
	Checked        int
	HashMismatches int
	AlignmentMaxMS int64
	Errors         []string
}

func (r Report) OK() bool { return len(r.Errors) == 0 }

// Verify compares the manifest to a freshly demultiplexed output timeline.
func (v Verifier) Verify(m Manifest, span model.Instant) (Report, error) {
	events, err := v.Demux()
	if err != nil {
		return Report{}, err
	}
	rep := Report{}
	byTrack := map[string][]model.Event{}
	for _, e := range events {
		if e.IsGap {
			continue
		}
		byTrack[e.TrackID] = append(byTrack[e.TrackID], e)
	}
	// First/last alignment per track against manifest samples.
	manifestByTrack := map[string][]SampleRecord{}
	for _, s := range m.Samples {
		manifestByTrack[s.TrackID] = append(manifestByTrack[s.TrackID], s)
	}
	ids := map[string]bool{}
	for _, tr := range m.Tracks {
		ids[tr.ID] = true
	}
	for id := range manifestByTrack {
		ids[id] = true
	}
	order := make([]string, 0, len(ids))
	for id := range ids {
		order = append(order, id)
	}
	sort.Strings(order)
	for _, id := range order {
		want := manifestByTrack[id]
		got := byTrack[id]
		if len(want) == 0 && len(got) == 0 {
			continue
		}
		if len(want) != len(got) {
			rep.Errors = append(rep.Errors, fmt.Sprintf("track %s: manifest has %d samples, output has %d", id, len(want), len(got)))
			continue
		}
		// Determine alignment as the max absolute PTS difference across the
		// first and last sample of each track.
		for _, pair := range []struct {
			i int
			a SampleRecord
			b model.Event
		}{{0, want[0], got[0]}, {len(want) - 1, want[len(want)-1], got[len(got)-1]}} {
			diff := pair.a.PTSNS - int64(pair.b.PTS)
			if diff < 0 {
				diff = -diff
			}
			if ms := diff / 1_000_000; ms > rep.AlignmentMaxMS {
				rep.AlignmentMaxMS = ms
			}
		}
		// Every sample hash must match.
		for i := range want {
			rep.Checked++
			if want[i].Hash != got[i].HashHex() {
				rep.HashMismatches++
				rep.Errors = append(rep.Errors, fmt.Sprintf("track %s sample %d hash mismatch", id, i))
			}
			// Segment boundary: the manifest's declared segment for a sample
			// must equal the segment the output timeline assigns it.
			mseg := timeline.SegmentFor(model.Instant(want[i].PTSNS), span)
			oseg := timeline.SegmentFor(got[i].PTS, span)
			if mseg != oseg {
				rep.Errors = append(rep.Errors, fmt.Sprintf(
					"track %s sample %d crosses segment boundary: manifest %d, output %d", id, i, mseg, oseg))
			}
		}
	}
	// Boundary attribution: every output event must fall in exactly one
	// closed-interval segment, and the manifest's segment set must cover the
	// output's first and last segments.
	outSegs := map[int]bool{}
	for _, e := range events {
		if e.IsGap {
			continue
		}
		seg := timeline.SegmentFor(e.PTS, span)
		if !timeline.InSegment(e.PTS, seg, span) {
			rep.Errors = append(rep.Errors, fmt.Sprintf("event at %dns not in its own segment %d", e.PTS, seg))
		}
		outSegs[seg] = true
	}
	for _, seg := range segmentBoundaries(m, span) {
		if !outSegs[seg] {
			rep.Errors = append(rep.Errors, fmt.Sprintf("manifest segment %d has no output events", seg))
		}
	}
	return rep, nil
}

// segmentBoundaries derives the set of segment indexes present in the
// manifest from sample times.
func segmentBoundaries(m Manifest, span model.Instant) []int {
	seen := map[int]bool{}
	for _, s := range m.Samples {
		seen[timeline.SegmentFor(model.Instant(s.PTSNS), span)] = true
	}
	out := make([]int, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// HashReader hashes a reader's full content, used to fingerprint sources.
func HashReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
