// Package fingerprint derives per-segment content hashes over frame payloads
// and their timeline placement. Metadata is deliberately excluded so a label
// edit cannot masquerade as a content change.
package fingerprint

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"

	"medpipe/internal/clock"
	"medpipe/internal/model"
	"medpipe/internal/segment"
)

// SegmentFingerprint is the digest of one segment plus its descriptive
// fields.
type SegmentFingerprint struct {
	Index     int
	StartNS   int64
	EndNS     int64
	Frames    int
	SHA256    string
	// DedupeKey identifies identical content across segments and tracks.
	// It hashes content digests only, not timing, so a re-timed duplicate is
	// still found; the full SHA256 additionally binds timing.
	DedupeKey string
}

// Result is the full fingerprint pass.
type Result struct {
	Segments []SegmentFingerprint
	// Canonical is the digest over the entire ordered fingerprint list.
	Canonical [32]byte
}

// Compute fingerprints every segment. Frames are addressed through the
// timeline so the same payload placed at a different time produces a
// different SHA256 but the same DedupeKey.
func Compute(tl *clock.Timeline, segments []segment.Segment, frames map[string][]model.Frame) (*Result, error) {
	if tl == nil {
		return nil, model.New(model.KindInput, "fingerprint.Compute", "nil timeline")
	}
	tracks := make(map[string]*clock.TrackTimeline, len(tl.Tracks))
	for i := range tl.Tracks {
		tracks[tl.Tracks[i].Track.ID] = &tl.Tracks[i]
	}
	res := &Result{Segments: make([]SegmentFingerprint, 0, len(segments))}
	canon := sha256.New()
	var b [8]byte
	for _, seg := range segments {
		fp, err := fingerprintSegment(seg, tracks, frames)
		if err != nil {
			return nil, err
		}
		res.Segments = append(res.Segments, fp)
		binary.BigEndian.PutUint64(b[:], uint64(seg.Index))
		canon.Write(b[:])
		canon.Write([]byte(fp.SHA256))
	}
	copy(res.Canonical[:], canon.Sum(nil))
	return res, nil
}

func fingerprintSegment(seg segment.Segment, tracks map[string]*clock.TrackTimeline, frames map[string][]model.Frame) (SegmentFingerprint, error) {
	fp := SegmentFingerprint{Index: seg.Index, StartNS: seg.StartNS, EndNS: seg.EndNS}
	h := sha256.New()
	h.Write([]byte("medpipe/segment/v1\x00"))
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(seg.StartNS))
	h.Write(b[:])
	binary.BigEndian.PutUint64(b[:], uint64(seg.EndNS))
	h.Write(b[:])

	dedupe := sha256.New()
	dedupe.Write([]byte("medpipe/dedupe/v1\x00"))
	slices := append([]segment.TrackSlice(nil), seg.Slices...)
	sort.SliceStable(slices, func(i, j int) bool { return slices[i].TrackID < slices[j].TrackID })
	for _, sl := range slices {
		tt, ok := tracks[sl.TrackID]
		if !ok {
			return fp, model.New(model.KindInvariant, "fingerprint.Compute",
				fmt.Sprintf("segment references unknown track %q", sl.TrackID))
		}
		fs := frames[sl.TrackID]
		if sl.FirstFrame < 0 || sl.FirstFrame+sl.FrameCount > len(fs) || sl.FirstFrame+sl.FrameCount > len(tt.Frames) {
			return fp, model.New(model.KindInvariant, "fingerprint.Compute",
				fmt.Sprintf("track %s slice [%d,%d) is out of range", sl.TrackID, sl.FirstFrame, sl.FirstFrame+sl.FrameCount))
		}
		h.Write([]byte(sl.TrackID))
		h.Write([]byte{0})
		binary.BigEndian.PutUint64(b[:], uint64(sl.FrameCount))
		h.Write(b[:])
		for k := 0; k < sl.FrameCount; k++ {
			idx := sl.FirstFrame + k
			fr := &fs[idx]
			// Timing is bound into the content hash through the frame's
			// timeline position relative to the segment start.
			rel := tt.Frames[idx].NS - seg.StartNS
			binary.BigEndian.PutUint64(b[:], uint64(rel))
			h.Write(b[:])
			d := fr.ContentDigest()
			h.Write(d[:])
			dedupe.Write(d[:])
			fp.Frames++
		}
	}
	sum := h.Sum(nil)
	fp.SHA256 = hex.EncodeToString(sum)
	fp.DedupeKey = hex.EncodeToString(dedupe.Sum(nil))
	return fp, nil
}

// Duplicates groups segment indexes by identical dedupe key. Groups of size
// one are omitted.
func Duplicates(segs []SegmentFingerprint) [][]int {
	byKey := map[string][]int{}
	for _, s := range segs {
		byKey[s.DedupeKey] = append(byKey[s.DedupeKey], s.Index)
	}
	var out [][]int
	for _, idxs := range byKey {
		if len(idxs) > 1 {
			sort.Ints(idxs)
			out = append(out, idxs)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}
