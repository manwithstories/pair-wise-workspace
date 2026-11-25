// Package index maintains keyframe (sync sample) indexes. When a source's stss
// is missing or corrupt it rebuilds the index by scanning coded samples,
// bounded by a strict byte budget so a damaged fragment cannot force an
// unbounded scan. An incomplete fragment never produces a phantom keyframe.
package index

import (
	"archivecheck/internal/demux"
	"archivecheck/internal/media"
)

// Keyframe records one sync sample's position for cut-point lookup.
type Keyframe struct {
	Track       int
	SampleIndex int // 0-based within the track
	Offset      int64
	Fragment    int
}

// RebuildReport explains how an index was obtained and what it cost.
type RebuildReport struct {
	Track       int
	Rebuilt     bool   // true when a scan replaced a missing/corrupt index
	Scanned     int64  // coded bytes inspected
	ScanLimited bool   // scan stopped at the byte budget
	Keyframes   int
	Faults      []*media.Fault
}

// ScanBudget caps the coded bytes inspected for one track's index rebuild.
const ScanBudget int64 = 4 << 20 // 4 MiB

// H264 NAL types that indicate a sync (keyframe) access unit.
const (
	nalIDR = 5
	nalSPS = 7
	nalPPS = 8
)

// IndexFor returns the authoritative keyframe index for a track. It trusts a
// well-formed stss-derived index; otherwise (absent, empty on a video track,
// or contradicted by the coded bytes) it rebuilds by a bounded scan.
func IndexFor(ts demux.TrackSamples, budget int64) ([]Keyframe, RebuildReport) {
	rep := RebuildReport{Track: ts.Meta.Track}
	if budget <= 0 {
		budget = ScanBudget
	}
	// Non-video tracks have no keyframe concept: an empty index is valid.
	if ts.Meta.Handler != "vide" {
		return nil, rep
	}

	stss := ts.Meta.Keyframes
	if stssHealthy(stss, len(ts.Samples)) && stssConsistent(stss, ts.Samples) {
		kf := make([]Keyframe, 0, len(stss))
		for _, oneBased := range stss {
			i := oneBased - 1
			if i >= 0 && i < len(ts.Samples) {
				kf = append(kf, keyframeOf(ts.Samples[i]))
			}
		}
		rep.Keyframes = len(kf)
		return kf, rep
	}

	if len(stss) > 0 {
		rep.Faults = append(rep.Faults, media.NewFault(media.KindCorruptIndex,
			"", ts.Meta.Track, -1, "stss contradicts coded bytes; rebuilding by scan"))
	} else {
		rep.Faults = append(rep.Faults, media.NewFault(media.KindMissingIndex,
			"", ts.Meta.Track, -1, "no stss present; rebuilding by scan"))
	}
	return rebuild(ts, budget, &rep)
}

// stssHealthy rejects absent/empty indexes on a populated video track and
// out-of-range or unsorted sample numbers.
func stssHealthy(stss []int, sampleCount int) bool {
	if sampleCount == 0 {
		return true
	}
	if len(stss) == 0 {
		return false
	}
	prev := 0
	for _, n := range stss {
		if n < 1 || n > sampleCount || n <= prev {
			return false
		}
		prev = n
	}
	return true
}

// stssConsistent verifies a sample the stss calls a sync actually looks like an
// IDR/SPS/PPS access unit in the coded bytes.
func stssConsistent(stss []int, samples []demux.Sample) bool {
	for _, oneBased := range stss {
		i := oneBased - 1
		if i < 0 || i >= len(samples) {
			return false
		}
		if !looksKey(samples[i].Data) {
			return false
		}
	}
	return true
}

// rebuild scans samples in order under the byte budget. The first sample of an
// incomplete fragment is never promoted: such samples arrive with Fragment set
// and the demuxer has already dropped the partial tail, so only fully-read
// samples participate.
func rebuild(ts demux.TrackSamples, budget int64, rep *RebuildReport) ([]Keyframe, RebuildReport) {
	rep.Rebuilt = true
	var kf []Keyframe
	var scanned int64
	for _, s := range ts.Samples {
		if scanned >= budget {
			rep.ScanLimited = true
			rep.Faults = append(rep.Faults, media.NewFault(media.KindScanLimit,
				"", ts.Meta.Track, s.Offset, "keyframe scan stopped at %d byte budget", budget))
			break
		}
		scanned += int64(len(s.Data))
		if looksKey(s.Data) {
			kf = append(kf, keyframeOf(s))
		}
	}
	rep.Scanned = scanned
	rep.Keyframes = len(kf)
	return kf, *rep
}

// looksKey inspects AVCC-framed H.264 (4-byte length + NAL) without decoding.
// A sample is a keyframe when one of its NAL units is IDR/SPS/PPS.
func looksKey(data []byte) bool {
	pos := 0
	for pos+4 <= len(data) {
		n := int(data[pos])<<24 | int(data[pos+1])<<16 | int(data[pos+2])<<8 | int(data[pos+3])
		pos += 4
		if n <= 0 || pos+n > len(data) {
			return false // malformed framing: do not claim a keyframe
		}
		if t := data[pos] & 0x1F; t == nalIDR || t == nalSPS || t == nalPPS {
			return true
		}
		pos += n
	}
	return false
}

func keyframeOf(s demux.Sample) Keyframe {
	return Keyframe{Track: s.Track, SampleIndex: s.Index, Offset: s.Offset, Fragment: s.Fragment}
}

// NearestKeyframeAtOrBefore returns the last keyframe whose sample index is <=
// target, supporting exact cut-point alignment. The boolean is false when no
// keyframe qualifies (a cut cannot start on a dependent sample).
func NearestKeyframeAtOrBefore(keyframes []Keyframe, target int) (Keyframe, bool) {
	var best Keyframe
	found := false
	for _, k := range keyframes {
		if k.SampleIndex <= target {
			best, found = k, true
		} else {
			break
		}
	}
	return best, found
}
