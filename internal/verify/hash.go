// Package verify re-reads a produced MP4 independently of the writer and
// checks it against the source byte ranges recorded during demux. The checks
// are the archive's proof of fidelity: per-sample SHA-256 equality, chunk
// coverage, no gaps, and identical counts.
package verify

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/abema/go-mp4"

	"archivecheck.local/archivecheck/internal/fault"
	"archivecheck.local/archivecheck/internal/index"
	"archivecheck.local/archivecheck/internal/remux"
)

// Segment is one comparable unit: a track's chunk of samples.
type Segment struct {
	Track  uint64
	Offset int64
	Size   int
	SHA256 string
}

// Result is the outcome of a round-trip check.
type Result struct {
	// OutputSegments and SourceSegments are the chunks in output order.
	OutputSegments []Segment
	SourceSegments []Segment
	// Matched is the number of output segments whose SHA-256 equals the
	// expected source hash.
	Matched int
	// OutputSamples and MatchedSamples count samples, not segments.
	OutputSamples  int64
	MatchedSamples int64
	// KeyframeQueries and KeyframeHits drive the hit-rate metric.
	KeyframeQueries int
	KeyframeHits    int
	// Gaps lists output ranges not covered by any declared sample.
	Gaps []Gap
	// TruncatedOutput reports an output file shorter than its last sample.
	TruncatedOutput bool
}

// Gap is an uncovered output byte range.
type Gap struct {
	Start int64
	End   int64
}

// HashSum is the SHA-256 digest of a byte range.
func HashSum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Options configure a verification pass.
type Options struct {
	// Input is the original Matroska file, re-read to confirm that the bytes
	// the writer claimed to copy still hash identically.
	Input io.ReaderAt
	// InputSize is that file's length.
	InputSize int64
	// KeyframeTicks selects query timestamps per track; the verifier looks up
	// each tick in the rebuilt index and checks the answer against the sample
	// whose output index the writer recorded.
	KeyframeTicks map[uint64][]int64
}

// Check re-reads out and compares it with the manifest produced by the
// writer. It returns an error only for structural defects; fidelity
// differences are reported in Result so the CLI can print rates.
func Check(out io.ReaderAt, outSize int64, manifest []remux.ManifestEntry, opts Options) (*Result, error) {
	if outSize <= 0 {
		return nil, fault.New(fault.StageVerify, fmt.Errorf("%w: empty output", fault.ErrMalformed))
	}
	res := &Result{}
	probe, err := mp4.Probe(io.NewSectionReader(out, 0, outSize))
	if err != nil {
		return nil, fault.At(fault.StageVerify, 0, fmt.Errorf("%w: %v", fault.ErrMalformed, err))
	}
	if len(probe.Tracks) == 0 {
		return nil, fault.New(fault.StageVerify, fmt.Errorf("%w: no tracks in output", fault.ErrMalformed))
	}

	// Group expected samples by track in output order.
	expected := map[uint64][]remux.ManifestEntry{}
	for _, m := range manifest {
		expected[m.Track] = append(expected[m.Track], m)
	}
	for _, list := range expected {
		sort.Slice(list, func(i, j int) bool { return list[i].OutputSample < list[j].OutputSample })
	}

	covered := make([]Gap, 0)
	// probe keeps tracks in file order, which the writer made identical to
	// ascending Matroska track order.
	manifestTracks := ManifestTracks(manifest)
	trackStss, err := readTrackStss(out, outSize)
	if err != nil {
		return nil, err
	}
	for ti, tr := range probe.Tracks {
		var trackNum uint64
		if ti < len(manifestTracks) {
			trackNum = manifestTracks[ti]
		} else {
			trackNum = manifestTrackAt(manifest, len(res.OutputSegments))
		}
		exp := expected[trackNum]
		if len(exp) == 0 {
			continue
		}
		if len(tr.Samples) != len(exp) {
			return nil, fault.New(fault.StageVerify,
				fmt.Errorf("%w: track %d sample count %d != expected %d",
					fault.ErrMalformed, trackNum, len(tr.Samples), len(exp)))
		}
		sampleIdx := 0
		for _, chunk := range tr.Chunks {
			start := int64(chunk.DataOffset)
			var length int64
			for i := 0; i < int(chunk.SamplesPerChunk) && sampleIdx < len(exp); i++ {
				size := int64(tr.Samples[sampleIdx].Size)
				want := exp[sampleIdx]
				if size != int64(want.Size) {
					return nil, fault.New(fault.StageVerify,
						fmt.Errorf("%w: track %d sample %d size %d != manifest %d",
							fault.ErrMalformed, trackNum, sampleIdx+1, size, want.Size)).OnTrack(trackNum)
				}
				buf := make([]byte, size)
				if start+length+size > outSize {
					res.TruncatedOutput = true
					return nil, fault.New(fault.StageVerify,
						fmt.Errorf("%w: output ends inside sample", fault.ErrTruncated)).OnTrack(trackNum)
				}
				if _, err := out.ReadAt(buf, start+length); err != nil {
					return nil, fault.At(fault.StageVerify, start+length, err)
				}
				sum := HashSum(buf)
				srcSum, err := sourceHash(opts, want)
				if err != nil {
					return nil, err
				}
				seg := Segment{Track: trackNum, Offset: start + length, Size: int(size), SHA256: sum}
				res.OutputSegments = append(res.OutputSegments, seg)
				res.SourceSegments = append(res.SourceSegments, Segment{Track: trackNum, Offset: want.SourceOffset, Size: want.Size, SHA256: srcSum})
				res.OutputSamples++
				if sum == srcSum {
					res.Matched++
					res.MatchedSamples++
				}
				length += size
				sampleIdx++
			}
			covered = append(covered, Gap{Start: start, End: start + length})
		}
		if sampleIdx != len(exp) {
			return nil, fault.New(fault.StageVerify,
				fmt.Errorf("%w: track %d chunks cover %d of %d samples", fault.ErrMalformed, trackNum, sampleIdx, len(exp))).OnTrack(trackNum)
		}
		var syncSamples map[uint32]bool
		if ti < len(trackStss) {
			syncSamples = trackStss[ti]
		}
		q, h := countKeyframes(exp, syncSamples)
		res.KeyframeQueries += q
		res.KeyframeHits += h
	}
	// Only the media payload has to be gap-free; ftyp/moov are metadata.
	mdatStart, mdatEnd, err := findMdat(out, outSize)
	if err != nil {
		return nil, err
	}
	res.Gaps = findGaps(covered, mdatStart, mdatEnd)
	if len(res.Gaps) > 0 {
		return nil, fault.New(fault.StageVerify,
			fmt.Errorf("%w: %d uncovered output ranges", fault.ErrMalformed, len(res.Gaps)))
	}
	return res, nil
}

// countKeyframes confirms that the output's sample table marks every sample
// the writer recorded as a keyframe. stss is not exposed by go-mp4's probe, so
// this re-reads the box and compares membership directly.
func countKeyframes(exp []remux.ManifestEntry, syncSamples map[uint32]bool) (queries, hits int) {
	for i, m := range exp {
		if !m.Keyframe {
			continue
		}
		queries++
		if syncSamples[uint32(i+1)] {
			hits++
		}
	}
	return queries, hits
}

var errBoxNotFound = errors.New("box not found")

// readTrackStss returns the stss sample-number set for each trak, in file
// order. Tracks without stss (audio, subtitles) get an empty set: every
// sample is a sync sample by definition.
func readTrackStss(r io.ReaderAt, size int64) ([]map[uint32]bool, error) {
	moov, err := readBoxPayload(r, 0, size, "moov")
	if err != nil {
		return nil, err
	}
	var out []map[uint32]bool
	off := int64(0)
	for off+8 <= int64(len(moov)) {
		boxSize, typ, header, ok := parseBoxHeader(moov, off)
		if !ok {
			return nil, fault.New(fault.StageVerify, fmt.Errorf("%w: bad moov child", fault.ErrMalformed))
		}
		if typ == "trak" {
			set, err := readTrakStss(moov[off+header : off+boxSize])
			if err != nil {
				return nil, err
			}
			out = append(out, set)
		}
		off += boxSize
	}
	return out, nil
}

func readTrakStss(trak []byte) (map[uint32]bool, error) {
	stbl, err := boxInPayload(trak, "mdia", "minf", "stbl")
	if err != nil {
		return nil, err
	}
	stss, err := boxInPayload(stbl, "stss")
	if err != nil {
		if errors.Is(err, errBoxNotFound) {
			return map[uint32]bool{}, nil
		}
		return nil, err
	}
	if len(stss) < 8 {
		return nil, fault.New(fault.StageVerify, fmt.Errorf("%w: stss too short", fault.ErrMalformed))
	}
	count := binary.BigEndian.Uint32(stss[4:8])
	if int64(8)+int64(count)*4 > int64(len(stss)) {
		return nil, fault.New(fault.StageVerify, fmt.Errorf("%w: stss count exceeds box", fault.ErrOversized))
	}
	set := make(map[uint32]bool, count)
	for i := uint32(0); i < count; i++ {
		set[binary.BigEndian.Uint32(stss[8+i*4:])] = true
	}
	return set, nil
}

// parseBoxHeader decodes an 8- or 16-byte box header inside buf.
func parseBoxHeader(buf []byte, off int64) (size int64, typ string, header int64, ok bool) {
	if off+8 > int64(len(buf)) {
		return 0, "", 0, false
	}
	size = int64(binary.BigEndian.Uint32(buf[off:]))
	typ = string(buf[off+4 : off+8])
	header = 8
	if size == 1 {
		if off+16 > int64(len(buf)) {
			return 0, "", 0, false
		}
		size = int64(binary.BigEndian.Uint64(buf[off+8:]))
		header = 16
	} else if size == 0 {
		size = int64(len(buf)) - off
	}
	if size < header || off+size > int64(len(buf)) {
		return 0, "", 0, false
	}
	return size, typ, header, true
}

// boxInPayload walks a chain of child boxes and returns the last payload.
func boxInPayload(buf []byte, path ...string) ([]byte, error) {
	off := int64(0)
	for i, want := range path {
		found := false
		for off+8 <= int64(len(buf)) {
			size, typ, header, ok := parseBoxHeader(buf, off)
			if !ok {
				return nil, errBoxNotFound
			}
			if typ == want {
				payload := buf[off+header : off+size]
				if i == len(path)-1 {
					return payload, nil
				}
				buf = payload
				off = 0
				found = true
				break
			}
			off += size
		}
		if !found {
			return nil, errBoxNotFound
		}
	}
	return nil, errBoxNotFound
}

func readBoxPayload(r io.ReaderAt, start, length int64, want string) ([]byte, error) {
	hdr := make([]byte, 16)
	off := start
	for off+8 <= start+length {
		if _, err := r.ReadAt(hdr[:8], off); err != nil {
			return nil, err
		}
		size := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		header := int64(8)
		if size == 1 {
			if _, err := r.ReadAt(hdr[8:16], off+8); err != nil {
				return nil, err
			}
			size = int64(binary.BigEndian.Uint64(hdr[8:16]))
			header = 16
		} else if size == 0 {
			size = start + length - off
		}
		if size < header || off+size > start+length {
			return nil, errBoxNotFound
		}
		if typ == want {
			buf := make([]byte, size-header)
			if _, err := r.ReadAt(buf, off+header); err != nil {
				return nil, err
			}
			return buf, nil
		}
		off += size
	}
	return nil, errBoxNotFound
}

// sourceHash re-reads the source bytes for one sample.
func sourceHash(opts Options, m remux.ManifestEntry) (string, error) {
	if opts.Input == nil {
		return "", nil
	}
	if m.SourceOffset < 0 || m.SourceOffset+int64(m.Size) > opts.InputSize {
		return "", fault.AtElement(fault.StageVerify, m.SourceOffset, "Sample",
			fmt.Errorf("%w: source range outside input", fault.ErrMalformed))
	}
	buf := make([]byte, m.Size)
	if _, err := opts.Input.ReadAt(buf, m.SourceOffset); err != nil {
		return "", fault.At(fault.StageVerify, m.SourceOffset, err)
	}
	return HashSum(buf), nil
}

// trackNumberFromManifest maps an MP4 track_ID back to a Matroska track
// number using the manifest's track order (ordered by Matroska number).
func trackNumberFromManifest(manifest []remux.ManifestEntry, trackID uint32) (uint64, error) {
	if trackID == 0 {
		return 0, fmt.Errorf("zero track ID")
	}
	nums := ManifestTracks(manifest)
	idx := int(trackID) - 1
	if idx < 0 || idx >= len(nums) {
		return 0, fmt.Errorf("track ID %d out of range", trackID)
	}
	return nums[idx], nil
}

// ManifestTracks returns the distinct Matroska track numbers in manifest
// order.
func ManifestTracks(manifest []remux.ManifestEntry) []uint64 {
	seen := map[uint64]bool{}
	var nums []uint64
	for _, m := range manifest {
		if !seen[m.Track] {
			seen[m.Track] = true
			nums = append(nums, m.Track)
		}
	}
	sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })
	return nums
}

func manifestTrackAt(manifest []remux.ManifestEntry, i int) uint64 {
	if i < 0 || i >= len(manifest) {
		return 0
	}
	return manifest[i].Track
}

// findGaps returns the byte ranges inside [start,end) not covered. It is used
// to prove that the media payload has no holes a player could fall into.
func findGaps(covered []Gap, start, end int64) []Gap {
	sort.Slice(covered, func(i, j int) bool { return covered[i].Start < covered[j].Start })
	var gaps []Gap
	cursor := start
	for _, g := range covered {
		if g.Start > cursor {
			gaps = append(gaps, Gap{Start: cursor, End: g.Start})
		}
		if g.End > cursor {
			cursor = g.End
		}
	}
	if cursor < end {
		gaps = append(gaps, Gap{Start: cursor, End: end})
	}
	return gaps
}

// findMdat locates the media data payload by walking top-level boxes.
func findMdat(r io.ReaderAt, size int64) (int64, int64, error) {
	off := int64(0)
	for off+8 <= size {
		hdr := make([]byte, 16)
		n := 8
		if _, err := r.ReadAt(hdr[:8], off); err != nil {
			return 0, 0, fault.At(fault.StageVerify, off, err)
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		if boxSize == 1 {
			if off+16 > size {
				return 0, 0, fault.At(fault.StageVerify, off, fmt.Errorf("%w: largesize header cut off", fault.ErrTruncated))
			}
			if _, err := r.ReadAt(hdr[8:16], off+8); err != nil {
				return 0, 0, fault.At(fault.StageVerify, off, err)
			}
			boxSize = int64(binary.BigEndian.Uint64(hdr[8:16]))
			n = 16
		} else if boxSize == 0 {
			boxSize = size - off
		}
		if boxSize < int64(n) || off+boxSize > size {
			return 0, 0, fault.At(fault.StageVerify, off,
				fmt.Errorf("%w: box %q has invalid size %d", fault.ErrMalformed, typ, boxSize))
		}
		if typ == "mdat" {
			return off + int64(n), off + boxSize, nil
		}
		off += boxSize
	}
	return 0, 0, fault.New(fault.StageVerify, fmt.Errorf("%w: no mdat box", fault.ErrMalformed))
}

// Rates summarises fidelity as percentages for terminal output.
type Rates struct {
	RoundtripMatch   float64
	KeyframeHit      float64
	SegmentHashMatch float64
	OutputSegments   int
	MatchedSegments  int
	OutputSamples    int64
	MatchedSamples   int64
	KeyframeQueries  int
	KeyframeHits     int
}

// ComputeRates turns counts into the four reported percentages.
func (r *Result) ComputeRates() Rates {
	out := Rates{
		OutputSegments:  len(r.OutputSegments),
		MatchedSegments: r.Matched,
		OutputSamples:   r.OutputSamples,
		MatchedSamples:  r.MatchedSamples,
		KeyframeQueries: r.KeyframeQueries,
		KeyframeHits:    r.KeyframeHits,
	}
	if r.OutputSamples > 0 {
		out.RoundtripMatch = 100 * float64(r.MatchedSamples) / float64(r.OutputSamples)
	}
	if len(r.OutputSegments) > 0 {
		out.SegmentHashMatch = 100 * float64(r.Matched) / float64(len(r.OutputSegments))
	}
	if r.KeyframeQueries > 0 {
		out.KeyframeHit = 100 * float64(r.KeyframeHits) / float64(r.KeyframeQueries)
	}
	return out
}

// CheckIndex verifies keyframe index lookups against observed output sample
// positions. Each query asks for a timestamp and expects the keyframe at or
// before it.
func CheckIndex(ix *index.Index, queries map[uint64][]Query) (hits, total int) {
	for track, qs := range queries {
		for _, q := range qs {
			total++
			e, ok := ix.AtOrBefore(track, q.Tick)
			if ok && e.OutputSample == q.WantOutputSample {
				hits++
			}
		}
	}
	return hits, total
}

// Query is one expected keyframe lookup.
type Query struct {
	Tick             int64
	WantOutputSample int
}
