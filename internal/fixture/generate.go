// Package fixture generates deterministic MP4, subtitle and RTP
// samples for the demo and for tests: valid media, damaged subtitles,
// wrapping RTP timestamps, variable frame rate, duplicate PTS,
// truncated sample tables and node clock drift.
package fixture

import (
	"bytes"
	"fmt"
	"math/rand"

	"pairflow/media/internal/clock"
	"pairflow/media/internal/mp4"
)

// Options controls fixture generation. Every field is reproducible:
// the same Options always produce byte-identical output.
type Options struct {
	Seed       int64  `json:"seed"`
	DriftPPM   int64  `json:"drift_ppm"`
	WrapAt     uint32 `json:"wrap_at"`
	FrameRate  Rate   `json:"frame_rate"`
	FrameCount int    `json:"frame_count"`
}

// DefaultOptions returns the options used by the acceptance demo.
func DefaultOptions() Options {
	return Options{
		Seed:       41,
		DriftPPM:   85,
		WrapAt:     4294967000,
		FrameRate:  RateDrop2997,
		FrameCount: 120,
	}
}

// RTPPacket is one synthetic RTP header plus its ground-truth source
// and node tick values (in 90 kHz units).
type RTPPacket struct {
	Sequence    uint16 `json:"sequence"`
	Timestamp   uint32 `json:"timestamp"`
	SourceTicks int64  `json:"source_ticks"`
	NodeTicks   int64  `json:"node_ticks"`
	Frame       int    `json:"frame"`
}

// Set is a complete, in-memory fixture corpus.
type Set struct {
	Options Options `json:"options"`

	Master       []byte `json:"-"`
	TruncatedMP4 []byte `json:"-"`
	DuplicatePTS []byte `json:"-"`
	OutOfBounds  []byte `json:"-"`

	SRT []byte `json:"-"`
	VTT []byte `json:"-"`

	RTP []RTPPacket `json:"rtp"`

	VideoTimescale uint32         `json:"video_timescale"`
	VideoDeltas    []uint32       `json:"video_deltas"`
	VideoKeys      []bool         `json:"video_keys"`
	AudioDeltas    []uint32       `json:"audio_deltas"`
	FrameDuration  clock.Rational `json:"frame_duration"`
}

// Generate builds the full corpus. Drift is applied in integer
// arithmetic only: node = source + offset + source*ppm/1e6, with
// round-half-away-from-zero on the drift term.
func Generate(opts Options) (*Set, error) {
	if opts.FrameCount <= 0 {
		return nil, fmt.Errorf("fixture: frame count must be positive")
	}
	if opts.FrameRate.Num <= 0 || opts.FrameRate.Den <= 0 {
		opts.FrameRate = RateDrop2997
	}
	if opts.WrapAt == 0 {
		opts.WrapAt = 4294967000
	}
	rng := rand.New(rand.NewSource(opts.Seed))

	set := &Set{Options: opts, VideoTimescale: 30000, FrameDuration: opts.FrameRate.FrameDuration()}

	// Video deltas: mostly 1001, sprinkled with longer holds to make the
	// track genuinely variable frame rate. Every 12th sample is a sync
	// sample.
	n := opts.FrameCount
	deltas := make([]uint32, n)
	keys := make([]bool, n)
	base := uint32(30000 * opts.FrameRate.Den / (opts.FrameRate.Num * 1000))
	if base == 0 {
		base = 1001
	}
	for i := 0; i < n; i++ {
		deltas[i] = base
		if i > 0 && i%7 == 0 {
			deltas[i] = base * 2
		}
		keys[i] = i%12 == 0
	}
	// Reproducibility check: consume the RNG only after fixed layout so
	// seed changes never alter the schema.
	_ = rng.Int63()
	set.VideoDeltas = deltas
	set.VideoKeys = keys

	// Audio: 48 kHz, 1024-sample AAC frames.
	audioFrames := n * 2
	audioDeltas := make([]uint32, audioFrames)
	for i := range audioDeltas {
		audioDeltas[i] = 1024
	}
	set.AudioDeltas = audioDeltas

	video := mp4.Track{
		ID: 1, Handler: "vide", Timescale: set.VideoTimescale,
		Deltas: deltas, Keys: keys, SampleSize: 64,
	}
	audio := mp4.Track{
		ID: 2, Handler: "soun", Timescale: 48000,
		Deltas: audioDeltas, SampleSize: 32,
	}
	var b mp4.Builder
	b.Add(video)
	b.Add(audio)
	valid, err := b.Build()
	if err != nil {
		return nil, err
	}
	set.Master = valid.Bytes

	// Damaged variants: truncated stsz table, duplicate PTS (written by
	// rewriting stts deltas to zero at two positions), out-of-bounds
	// chunk offset.
	var tb mp4.Builder
	tb.OmitSizes = true
	tb.Add(video)
	tb.Add(audio)
	truncated, err := tb.Build()
	if err != nil {
		return nil, err
	}
	set.TruncatedMP4 = truncated.Bytes

	dupDeltas := append([]uint32(nil), deltas...)
	if len(dupDeltas) > 4 {
		dupDeltas[2] = 0
		dupDeltas[3] = dupDeltas[1]
		dupDeltas[2] = 0
	}
	var db mp4.Builder
	db.Add(mp4.Track{ID: 1, Handler: "vide", Timescale: set.VideoTimescale,
		Deltas: dupDeltas, Keys: keys, SampleSize: 64})
	dup, err := db.Build()
	if err != nil {
		return nil, err
	}
	set.DuplicatePTS = dup.Bytes

	var ob mp4.Builder
	ob.ChunkOffsetBias = 1 << 24
	ob.Add(mp4.Track{ID: 1, Handler: "vide", Timescale: set.VideoTimescale,
		Deltas: deltas, Keys: keys, SampleSize: 64})
	oob, err := ob.Build()
	if err != nil {
		return nil, err
	}
	set.OutOfBounds = oob.Bytes

	set.RTP = buildRTP(opts, deltas, base)
	set.SRT = buildSRT(opts, deltas)
	set.VTT = buildVTT(opts, deltas)
	return set, nil
}

// buildRTP produces one RTP packet per video frame. The 90 kHz source
// timestamp is exact: frame start ticks * 90000 / 30000. The node clock
// runs fast by DriftPPM and starts 1.5 s late; the wire value is biased
// so the first packet lands at WrapAt and crosses 2^32 mid-stream.
func buildRTP(opts Options, deltas []uint32, baseDelta uint32) []RTPPacket {
	out := make([]RTPPacket, 0, len(deltas))
	const rtpBase = int64(90000)
	offsetTicks := int64(3) * rtpBase / 2
	var srcTick int64
	for i := range deltas {
		// Source ticks in the video timescale.
		src := srcTick * rtpBase / 30000
		drift := src * opts.DriftPPM / 1_000_000
		if src*opts.DriftPPM%1_000_000 >= 500_000 {
			drift++
		}
		node := src + offsetTicks + drift
		wire := uint32((int64(opts.WrapAt) + node) & 0xFFFFFFFF)
		out = append(out, RTPPacket{
			Sequence:    uint16(i),
			Timestamp:   wire,
			SourceTicks: src,
			NodeTicks:   node,
			Frame:       i,
		})
		srcTick += int64(deltas[i])
	}
	return out
}

// buildSRT writes a corpus with a legal overlap, one malformed timing
// line and a truncated tail record.
func buildSRT(opts Options, deltas []uint32) []byte {
	var b bytes.Buffer
	b.WriteString("1\r\n")
	b.WriteString("00:00:00,000 --> 00:00:01,200\r\n")
	b.WriteString("Opening caption\r\n\r\n")

	b.WriteString("2\r\n")
	b.WriteString("00:00:01,000 --> 00:00:02,000\r\n")
	b.WriteString("Legal overlap with cue 1\r\n\r\n")

	b.WriteString("3\r\n")
	b.WriteString("00:00:0x,000 --> 00:00:03,000\r\n")
	b.WriteString("damaged timing line\r\n\r\n")

	b.WriteString("4\r\n")
	b.WriteString("00:00:03,500 --> 00:00:04,500\r\n")
	b.WriteString("Recovered after damage\r\n\r\n")

	b.WriteString("5\r\n")
	b.WriteString("00:00:05,000 --> 00:00:06,000\r\n")
	b.WriteString("tail line without newline")
	return b.Bytes()
}

// buildVTT writes a BOM-prefixed, CRLF-delimited document with a NOTE
// block and one unparseable cue.
func buildVTT(opts Options, deltas []uint32) []byte {
	var b bytes.Buffer
	b.Write([]byte{0xEF, 0xBB, 0xBF})
	b.WriteString("WEBVTT - pairflow fixture\r\n")
	b.WriteString("Kind: captions\r\n\r\n")
	b.WriteString("NOTE generated for seed\r\n")
	b.WriteString("and drift verification\r\n\r\n")
	b.WriteString("intro\r\n")
	b.WriteString("00:00:00.000 --> 00:00:01.200 align:start\r\n")
	b.WriteString("Opening caption\r\n\r\n")
	b.WriteString("00:00:03.500 --> 00:00:0z.000\r\n")
	b.WriteString("bad cue\r\n\r\n")
	b.WriteString("outro\r\n")
	b.WriteString("00:00:05.000 --> 00:00:06.000\r\n")
	b.WriteString("Closing caption\r\n")
	return b.Bytes()
}
