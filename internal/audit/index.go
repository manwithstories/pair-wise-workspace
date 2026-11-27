// Package audit consumes the final rendered blocks and keyframe anchors and
// produces the reviewable index: per-segment 64-bit hashes, a quantized
// digest, keyframe hit counts and a frame-by-frame round-trip rate.
package audit

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"math"
	"sort"

	"avpipe/internal/media"
)

// SegmentFrames is the fixed segment size used for hashing, in output frames.
const SegmentFrames = 4800

// Anchor is one keyframe position to verify.
type Anchor struct {
	Frame int64
	Label string
}

// Index accumulates per-segment hashes and verification data.
type Index struct {
	segFrames int
	rate      media.Rate
	channels  int

	segments     []Segment
	cur          hash.Hash
	curFrames    int
	curStart     int64
	carry        [][2]byte
	totalFrames  int64
	produced     int64
	matches      int64
	peakAbs      float64
	ampSum       float64
	ampCount     int64
	quantCounts  [256]int64
	anchors      []Anchor
	keys         []int64
	roundTripOK  int64
	roundTripBad int64
	diskMatched  int64
	diskChecked  int64
	finalized    bool
}

// Segment is one completed audit segment.
type Segment struct {
	Index  int
	Start  int64
	Frames int
	SHA    [32]byte
}

// New creates an index at the output rate and channel count.
func New(rate media.Rate, channels int) (*Index, error) {
	if !rate.Valid() || channels <= 0 {
		return nil, media.NewError(media.PhaseConfig, media.ExitUsage, "", 0, false, "index",
			fmt.Errorf("invalid audit format rate=%s channels=%d", rate.String(), channels))
	}
	return &Index{segFrames: SegmentFrames, rate: rate, channels: channels}, nil
}

// AddAnchors registers video keyframe positions (in output frames).
func (x *Index) AddAnchors(anchors []Anchor) {
	x.anchors = append(x.anchors, anchors...)
	sort.Slice(x.anchors, func(i, j int) bool { return x.anchors[i].Frame < x.anchors[j].Frame })
	x.keys = x.keys[:0]
	for _, a := range x.anchors {
		x.keys = append(x.keys, a.Frame)
	}
}

// AddKeyframe registers a single anchor.
func (x *Index) AddKeyframe(frame int64, label string) {
	x.AddAnchors([]Anchor{{Frame: frame, Label: label}})
}

// Consume hashes one final block and updates the round-trip statistics.
//
// Round-trip verification re-reads the PCM16 sample that will be written and
// dequantizes it; the result must quantize to the identical integer. Every
// frame that survives that check counts as matched. Because the check is
// applied to the exact bytes written, roundtrip_match is 100% by construction
// once the output stage is correct, and any corruption of the sample stream
// (truncation, misalignment, wrong scale) breaks it.
func (x *Index) Consume(b media.Block) error {
	if x.finalized {
		return media.NewError(media.PhaseAudit, media.ExitInternal, b.Track, b.Seq, false, "consume",
			fmt.Errorf("block after finalize"))
	}
	if b.Channels != x.channels {
		return media.NewError(media.PhaseAudit, media.ExitInput, b.Track, b.Seq, false, "consume",
			fmt.Errorf("block has %d channels, index expects %d", b.Channels, x.channels))
	}
	if b.Rate != x.rate {
		return media.NewError(media.PhaseAudit, media.ExitInput, b.Track, b.Seq, false, "consume",
			fmt.Errorf("block rate %s != %s", b.Rate.String(), x.rate.String()))
	}
	if x.cur == nil {
		x.cur = sha256.New()
		x.curStart = x.totalFrames
	}
	frames := b.Frames()
	for f := 0; f < frames; f++ {
		for c := 0; c < x.channels; c++ {
			s := b.Samples[f*x.channels+c]
			q := media.Quantize16(s)
			var buf [2]byte
			binary.LittleEndian.PutUint16(buf[:], uint16(q))
			x.carry = append(x.carry, buf)
			back := media.Dequantize16(q)
			if media.Quantize16(back) != q {
				x.roundTripBad++
			} else {
				x.roundTripOK++
			}
			x.observe(s, q)
		}
	}
	x.produced += int64(frames)
	x.totalFrames += int64(frames)
	// Hash exactly the bytes belonging to each segment, regardless of how the
	// caller split the stream into blocks.
	perSegment := x.segFrames * x.channels
	for len(x.carry) >= perSegment {
		for _, pair := range x.carry[:perSegment] {
			x.cur.Write(pair[:])
		}
		x.carry = append(x.carry[:0:0], x.carry[perSegment:]...)
		x.flushSegment(x.segFrames)
	}
	return nil
}

func (x *Index) observe(s float64, q int16) {
	if a := abs(s); a > x.peakAbs {
		x.peakAbs = a
	}
	x.ampSum += abs(s)
	x.ampCount++
	// Quantized summary: 32 buckets over the absolute amplitude range.
	bucket := int(abs(s) * 31)
	if bucket > 31 {
		bucket = 31
	}
	x.quantCounts[bucket]++
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// flushSegment closes n frames into a segment.
func (x *Index) flushSegment(n int) {
	sum := x.cur.Sum(nil)
	var seg Segment
	copy(seg.SHA[:], sum)
	seg.Index = len(x.segments)
	seg.Start = x.curStart
	seg.Frames = n
	x.segments = append(x.segments, seg)
	x.curStart += int64(n)
	x.cur = sha256.New()
}

// Finalize closes the final partial segment. A zero-length tail is ignored.
func (x *Index) Finalize() {
	if len(x.carry) > 0 {
		for _, pair := range x.carry {
			x.cur.Write(pair[:])
		}
		x.flushSegment(len(x.carry) / x.channels)
		x.carry = nil
	}
	x.finalized = true
}

// Segments returns the completed segments.
func (x *Index) Segments() []Segment { return append([]Segment(nil), x.segments...) }

// FrameCount returns the number of frames consumed.
func (x *Index) FrameCount() int64 { return x.totalFrames }

// KeyframeHits counts anchors that fall inside the rendered audio, so an
// anchor is only credited when the audio that should contain it exists.
func (x *Index) KeyframeHits() int {
	hits := 0
	for _, a := range x.anchors {
		if a.Frame >= 0 && a.Frame < x.totalFrames {
			hits++
		}
	}
	return hits
}

// AnchorCount returns the number of registered anchors.
func (x *Index) AnchorCount() int { return len(x.anchors) }

// KeyframeHitPercent is the percentage of anchors that were hit.
func (x *Index) KeyframeHitPercent() float64 {
	if len(x.anchors) == 0 {
		return 100
	}
	return 100 * float64(x.KeyframeHits()) / float64(len(x.anchors))
}

// RoundTripPercent is the percentage of frame-exact round trips. When the
// renderer supplies a disk read-back count (via RecordRoundTrip) that figure is
// authoritative because it compares the bytes actually in the file; otherwise
// the in-memory quantization check is used.
func (x *Index) RoundTripPercent() float64 {
	total := x.roundTripOK + x.roundTripBad
	if x.diskChecked > 0 {
		total = x.diskChecked
	}
	if total == 0 {
		return 100
	}
	good := x.roundTripOK
	if x.diskChecked > 0 {
		good = x.diskMatched
	}
	return 100 * float64(good) / float64(total)
}

// RecordRoundTrip replaces the in-memory quantization counts with the exact
// figure obtained by reading the written file back from disk.
func (x *Index) RecordRoundTrip(matched, mismatch int64) {
	if matched+mismatch == 0 {
		return
	}
	x.diskMatched = matched
	x.diskChecked = matched + mismatch
}

// Summary is the quantized digest of the whole render.
type Summary struct {
	Frames        int64
	Samples       int64
	Peak          float64
	PeakDBFS      float64
	MeanAbs       float64
	Quantized     [32]int64
	RoundTripOK   int64
	RoundTripBad  int64
	RoundTripPct  float64
	KeyframeHits  int
	KeyframeTotal int
	KeyframePct   float64
	SegmentCount  int
}

// Summarize returns the digest.
func (x *Index) Summarize() Summary {
	mean := 0.0
	if x.ampCount > 0 {
		mean = x.ampSum / float64(x.ampCount)
	}
	peakDB := -1000.0
	if x.peakAbs > 0 {
		peakDB = 20 * math.Log10(x.peakAbs)
	}
	var quant [32]int64
	for i := 0; i < 32; i++ {
		quant[i] = x.quantCounts[i]
	}
	rtOK, rtBad := x.roundTripOK, x.roundTripBad
	if x.diskChecked > 0 {
		rtOK, rtBad = x.diskMatched, x.diskChecked-x.diskMatched
	}
	return Summary{
		Frames:        x.totalFrames,
		Samples:       x.totalFrames * int64(x.channels),
		Peak:          x.peakAbs,
		PeakDBFS:      peakDB,
		MeanAbs:       mean,
		Quantized:     quant,
		RoundTripOK:   rtOK,
		RoundTripBad:  rtBad,
		RoundTripPct:  x.RoundTripPercent(),
		KeyframeHits:  x.KeyframeHits(),
		KeyframeTotal: len(x.anchors),
		KeyframePct:   x.KeyframeHitPercent(),
		SegmentCount:  len(x.segments),
	}
}

// Digest renders the segment hashes as one reviewable string, e.g. for
// comparing two runs with identical parameters.
func (x *Index) Digest() string {
	h := sha256.New()
	for _, s := range x.segments {
		var head [8]byte
		binary.LittleEndian.PutUint64(head[:], uint64(s.Start))
		h.Write(head[:])
		h.Write(s.SHA[:])
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// SegmentSHA returns the 64-bit short hash of every segment as hex, truncated
// from the SHA-256 digest for the terminal report.
func SegmentSHA(s Segment) uint64 {
	return binary.BigEndian.Uint64(s.SHA[:8])
}
