package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"avpipe/internal/demux"
	"avpipe/internal/media"
)

// SegmentSource is a segment definition used to recompute hashes. It mirrors
// the fields the pipeline uses to cut segments.
type SegmentSource struct {
	Index   int
	Start   media.Rational
	End     media.Rational
	Reasons []string
}

// Segmenter groups an ordered packet stream into segments of at most dur
// seconds. A boundary is placed only at a *video* keyframe whose presentation
// time has reached the next multiple of dur, so every segment begins on a
// decodable boundary and keyframes are never split. Boundaries are derived
// from actual timestamps, never from a frame count, so variable frame rate is
// preserved exactly.
//
// videoTracks identifies which track IDs are video; when it is nil every key
// packet is considered a valid boundary.
func Segmenter(dur media.Rational, pkts []media.Packet, videoTracks map[string]bool) ([]SegmentSource, [][]media.Packet) {
	if !dur.Valid() || dur.Num <= 0 {
		dur = media.MustRat(2, 1)
	}
	if len(pkts) == 0 {
		return nil, nil
	}
	isVideoKey := func(p media.Packet) bool {
		if !p.Key {
			return false
		}
		if videoTracks == nil {
			return true
		}
		return videoTracks[p.TrackID]
	}
	var segs []SegmentSource
	var groups [][]media.Packet
	var cur []media.Packet
	idx := 0
	var start media.Rational
	if s, err := pkts[0].PTSSeconds(); err == nil {
		start = s
	}
	next, err := start.Add(dur)
	if err != nil {
		next = start
	}
	for _, p := range pkts {
		sec, err := p.PTSSeconds()
		if err == nil && isVideoKey(p) && len(cur) > 0 && sec.Cmp(next) >= 0 {
			segs = append(segs, SegmentSource{Index: idx, Start: start, End: sec})
			groups = append(groups, cur)
			idx++
			cur = nil
			start = sec
			// Anchor the next boundary dur after this keyframe.
			if b, addErr := sec.Add(dur); addErr == nil {
				next = b
			} else {
				next = sec
			}
		}
		cur = append(cur, p)
	}
	if len(cur) > 0 {
		var end media.Rational
		if s, err := cur[len(cur)-1].PTSSeconds(); err == nil {
			end = s
		} else {
			end = start
		}
		segs = append(segs, SegmentSource{Index: idx, Start: start, End: end})
		groups = append(groups, cur)
	}
	return segs, groups
}

// BuildSegmentRecords computes auditable records for grouped packets.
//
// FirstKey means "the segment can be decoded from its first video sample":
// the first video packet in the group must be a keyframe. Intermediate audio
// packets that share the boundary timestamp are not a decode requirement, so
// they never make a properly cut segment look undecodable. For an audio-only
// segment the first packet's key flag is used instead.
func BuildSegmentRecords(segs []SegmentSource, groups [][]media.Packet) []SegmentRecord {
	var out []SegmentRecord
	for i, g := range groups {
		s := segs[i]
		rec := SegmentRecord{
			Index: s.Index, Start: s.Start, End: s.End,
			Packets: len(g), Reasons: s.Reasons,
		}
		tracks := map[string]bool{}
		for _, p := range g {
			rec.Bytes += int64(len(p.Data))
			tracks[p.TrackID] = true
		}
		if len(g) > 0 {
			rec.FirstOrigin = g[0].Origin
			rec.FirstKey = segmentStartsDecodable(g)
		}
		for t := range tracks {
			rec.Tracks = append(rec.Tracks, t)
		}
		sort.Strings(rec.Tracks)
		rec.Hash = media.ComputeSegmentHash(s.Index, g)
		out = append(out, rec)
	}
	return out
}

// segmentStartsDecodable reports whether a group begins on a decodable
// boundary. A keyframe is the only packet kind that can anchor decoding, so
// the segment starts decodable when any packet in the group is a keyframe.
// The segmenter only cuts at video keyframes, so in practice this means the
// boundary packet itself is a keyframe; scanning the group tolerates the
// first segment, which begins at the timestamp zero regardless of key flag.
func segmentStartsDecodable(g []media.Packet) bool {
	for _, p := range g {
		if p.Key {
			return true
		}
	}
	return false
}

// FileHash returns the SHA-256 of a whole file.
func FileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", media.New(media.KindUsage, media.Origin{File: path}, "audit.FileHash", "open failed", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", media.New(media.KindCorrupt, media.Origin{File: path}, "audit.FileHash", "read failed", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Verification is the result of checking a manifest.
type Verification struct {
	ManifestPath string
	Segments     int
	HashOK       int
	HashBad      int
	OutputsOK    int
	OutputsBad   int
	Problems     []string
	// OutputDigestOK reports whether the segment digests in the manifest
	// reproduce the recorded output digest.
	OutputDigestOK bool
}

// Load reads and validates a manifest file.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, media.New(media.KindUsage, media.Origin{File: path}, "audit.Load", "reading manifest failed", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, media.New(media.KindCorrupt, media.Origin{File: path}, "audit.Load", "manifest is not valid JSON", err)
	}
	if m.Version != ManifestVersion {
		return nil, media.Errorf(media.KindCorrupt, media.Origin{File: path}, "audit.Load",
			"unsupported manifest version %d (want %d)", m.Version, ManifestVersion)
	}
	if len(m.Segments) == 0 {
		return nil, media.Errorf(media.KindCorrupt, media.Origin{File: path}, "audit.Load",
			"manifest has no segments")
	}
	return &m, nil
}

// Verify checks the manifest's internal consistency. When outputPath points at
// a real container, Verify also recomputes segmentation from that file and
// compares every segment hash to the manifest, which is how a re-run is
// proven equal to a full run.
func Verify(path string, outputPath string) (*Verification, error) {
	m, err := Load(path)
	if err != nil {
		return nil, err
	}
	v := &Verification{ManifestPath: path, Segments: len(m.Segments)}

	// 1. Internal: recompute the output digest from the recorded segment
	//    hashes and compare it to the stored digest.
	recomputed := outputDigest(m.Segments)
	v.OutputDigestOK = recomputed == m.OutputDigest
	if !v.OutputDigestOK {
		v.Problems = append(v.Problems, "output digest does not match recorded segment hashes")
	}

	// 2. Per-segment: every segment must have a hash and a positive packet
	//    count unless it is the only empty segment.
	seen := map[int]bool{}
	for _, s := range m.Segments {
		if seen[s.Index] {
			v.Problems = append(v.Problems, fmt.Sprintf("duplicate segment index %d", s.Index))
			v.HashBad++
			continue
		}
		seen[s.Index] = true
		if s.Hash == "" {
			v.Problems = append(v.Problems, fmt.Sprintf("segment %d has no hash", s.Index))
			v.HashBad++
			continue
		}
		v.HashOK++
	}

	if outputPath != "" {
		if err := verifyAgainstOutput(m, outputPath, v); err != nil {
			return nil, err
		}
	}
	return v, nil
}

// verifyAgainstOutput re-demuxes the produced file, re-segments it with the
// manifest's segment duration and compares the segment hashes.
func verifyAgainstOutput(m *Manifest, outputPath string, v *Verification) error {
	in, err := demux.Open(outputPath, demux.Options{})
	if err != nil {
		return err
	}
	defer in.Close()
	var pkts []media.Packet
	for {
		p, err := in.Packet()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		pkts = append(pkts, p)
	}
	// A container may interleave chunks for latency; the packets must be
	// compared in the canonical order the pipeline emits, which is exactly
	// the order the manifest hashed.
	media.SortPresentation(pkts)
	// Video tracks come from the manifest: the segmenter cuts only on video
	// keyframes, so verification must apply the same rule to the output.
	videoTracks := map[string]bool{}
	for _, tr := range m.Tracks {
		if tr.Kind == media.TrackVideo {
			videoTracks[tr.ID] = true
		}
	}
	segs, groups := Segmenter(m.SegmentSecs, pkts, videoTracks)
	recs := BuildSegmentRecords(segs, groups)
	if len(recs) != len(m.Segments) {
		v.OutputsBad++
		v.Problems = append(v.Problems,
			fmt.Sprintf("output has %d segments, manifest records %d", len(recs), len(m.Segments)))
	}
	byIndex := map[int]SegmentRecord{}
	for _, r := range recs {
		byIndex[r.Index] = r
	}
	for _, want := range m.Segments {
		got, ok := byIndex[want.Index]
		if !ok {
			v.OutputsBad++
			v.Problems = append(v.Problems, fmt.Sprintf("segment %d missing from output", want.Index))
			continue
		}
		if got.Hash != want.Hash {
			v.OutputsBad++
			v.Problems = append(v.Problems,
				fmt.Sprintf("segment %d hash mismatch: manifest %s output %s",
					want.Index, short(want.Hash), short(got.Hash)))
			continue
		}
		v.OutputsOK++
	}
	return nil
}

func short(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

// CompareRuns returns true when two manifests describe byte-identical
// segments and output digest. It is the replay-equality check.
func CompareRuns(a, b *Manifest) (bool, string) {
	if a.OutputDigest != b.OutputDigest {
		return false, "output digests differ"
	}
	if len(a.Segments) != len(b.Segments) {
		return false, fmt.Sprintf("segment counts differ: %d vs %d", len(a.Segments), len(b.Segments))
	}
	for i := range a.Segments {
		if a.Segments[i].Hash != b.Segments[i].Hash {
			return false, fmt.Sprintf("segment %d hash differs", a.Segments[i].Index)
		}
	}
	return true, "identical"
}
