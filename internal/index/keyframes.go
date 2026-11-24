// Package index builds a keyframe-based addressability index over a packet
// stream. With sparse keyframes only real sync samples are addressable cut
// points; ordinary packets are never reported as seek targets.
package index

import (
	"sort"

	"mediadeliver/internal/model"
)

// CutPoint is one addressable cut point: it references a specific packet and
// its source timestamp in the source track timescale.
type CutPoint struct {
	SourceID  string
	TrackID   uint32
	Seq       int64
	DTS       int64
	Timescale int64
}

// KeyframeIndex stores cut points per source track, sorted by DTS.
type KeyframeIndex struct {
	points map[string][]CutPoint // key: sourceID:trackID
	// total counts for hit-rate accounting
	totalPackets map[string]int64
	keyPackets   map[string]int64
}

// New returns an empty index.
func New() *KeyframeIndex {
	return &KeyframeIndex{
		points:       map[string][]CutPoint{},
		totalPackets: map[string]int64{},
		keyPackets:   map[string]int64{},
	}
}

func keyOf(sourceID string, trackID uint32) string {
	return sourceID + ":" + uitoa(trackID)
}

// Add records one packet. Only keyframes become cut points; the method never
// promotes a non-keyframe regardless of index sparsity.
func (k *KeyframeIndex) Add(p *model.Packet) {
	key := keyOf(p.SourceID, p.TrackID)
	k.totalPackets[key]++
	if !p.Keyframe {
		return
	}
	k.keyPackets[key]++
	k.points[key] = append(k.points[key], CutPoint{
		SourceID:  p.SourceID,
		TrackID:   p.TrackID,
		Seq:       p.Seq,
		DTS:       p.DTS,
		Timescale: p.Timescale,
	})
}

// Finalize sorts every track's cut points by DTS (insertion is normally
// ordered, but defensive sorting keeps lookup correct for multiplexed input).
func (k *KeyframeIndex) Finalize() {
	for key := range k.points {
		pts := k.points[key]
		sort.Slice(pts, func(i, j int) bool { return pts[i].DTS < pts[j].DTS })
		k.points[key] = pts
	}
}

// Lookup returns the greatest keyframe at or before targetDTS on the track.
// The boolean is false only when no keyframe precedes the target, which means
// the requested point is not addressable. Non-keyframe packets are never
// returned.
func (k *KeyframeIndex) Lookup(sourceID string, trackID uint32, targetDTS int64) (CutPoint, bool) {
	pts := k.points[keyOf(sourceID, trackID)]
	// binary search for the rightmost DTS <= target.
	idx := sort.Search(len(pts), func(i int) bool { return pts[i].DTS > targetDTS }) - 1
	if idx < 0 {
		return CutPoint{}, false
	}
	return pts[idx], true
}

// Points returns a copy of the cut points for a track.
func (k *KeyframeIndex) Points(sourceID string, trackID uint32) []CutPoint {
	src := k.points[keyOf(sourceID, trackID)]
	out := make([]CutPoint, len(src))
	copy(out, src)
	return out
}

// HitRate returns keyframe packets / total packets for a track.
func (k *KeyframeIndex) HitRate(sourceID string, trackID uint32) float64 {
	key := keyOf(sourceID, trackID)
	total := k.totalPackets[key]
	if total == 0 {
		return 0
	}
	return float64(k.keyPackets[key]) / float64(total)
}

// OverallHitRate aggregates over all indexed tracks.
func (k *KeyframeIndex) OverallHitRate() float64 {
	var total, keys int64
	for _, n := range k.totalPackets {
		total += n
	}
	for _, n := range k.keyPackets {
		keys += n
	}
	if total == 0 {
		return 0
	}
	return float64(keys) / float64(total)
}

// Counts returns total and keyframe packet counts for a track.
func (k *KeyframeIndex) Counts(sourceID string, trackID uint32) (total, keyframes int64) {
	key := keyOf(sourceID, trackID)
	return k.totalPackets[key], k.keyPackets[key]
}

func uitoa(v uint32) string {
	if v == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
