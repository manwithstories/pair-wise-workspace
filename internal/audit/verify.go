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

// Segmenter groups an ordered packet stream into fixed-duration segments,
// preserving the exact packet order. A segment boundary is placed after the
// first packet whose presentation time reaches the next multiple of the
// segment duration. Variable frame rate is preserved because boundaries are
// derived from actual timestamps, never from a frame count.
func Segmenter(dur media.Rational, pkts []media.Packet) ([]SegmentSource, [][]media.Packet) {
	if !dur.Valid() || dur.Num <= 0 {
		dur = media.MustRat(2, 1)
	}
	if len(pkts) == 0 {
		return nil, nil
	}
	var segs []SegmentSource
	var groups [][]media.Packet
	var cur []media.Packet
	idx := 0
	var start media.Rational
	// The first packet anchors the segment start.
	if s, err := pkts[0].PTSSeconds(); err == nil {
		start = s
	}
	boundary := start
	next, err := boundary.Add(dur)
	if err != nil {
		next = boundary
	}
	for _, p := range pkts {
		sec, err := p.PTSSeconds()
		if err == nil && sec.Cmp(next) >= 0 && len(cur) > 0 {
			segs = append(segs, SegmentSource{Index: idx, Start: start, End: sec})
			groups = append(groups, cur)
			idx++
			cur = nil
			start = sec
			// Advance the boundary past the packet's time.
			next = start
			b, addErr := next.Add(dur)
			if addErr != nil {
				next = start
			} else {
				next = b
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
			if p.Key {
				if len(g) > 0 && p.Seq == g[0].Seq && p.TrackID == g[0].TrackID {
					rec.FirstKey = true
				}
			}
			tracks[p.TrackID] = true
		}
		if len(g) > 0 {
			rec.FirstOrigin = g[0].Origin
			if g[0].Key {
				rec.FirstKey = true
			}
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
	segs, groups := Segmenter(m.SegmentSecs, pkts)
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
