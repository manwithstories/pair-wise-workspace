package pipeline

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand"
	"os"
	"path/filepath"

	"github.com/pion/rtp"

	"mediatimeline/internal/bitstream"
	"mediatimeline/internal/clock"
)

// Generate creates a deterministic set of input fixtures (elementary
// streams, keyframe indexes, RTP captures, anchor files) plus run.json
// and inputs/tracks.json in dir. The same params always produce
// byte-identical fixtures.
func Generate(dir string, params RunParams) (*FixtureMeta, error) {
	if params.MaxDriftPPM <= 0 {
		params.MaxDriftPPM = 500
	}
	inputs := filepath.Join(dir, "inputs")
	if err := os.MkdirAll(inputs, 0o755); err != nil {
		return nil, err
	}
	meta := &FixtureMeta{Params: params, CorruptOffset: -1}
	for t := 0; t < params.Tracks; t++ {
		spec, corruptOff, err := genTrack(inputs, params, t)
		if err != nil {
			return nil, err
		}
		meta.Tracks = append(meta.Tracks, *spec)
		if corruptOff >= 0 {
			meta.CorruptTrack = spec.ID
			meta.CorruptOffset = corruptOff
		}
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(inputs, "tracks.json"), data, 0o644); err != nil {
		return nil, err
	}
	runData, err := json.MarshalIndent(params, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "run.json"), runData, 0o644); err != nil {
		return nil, err
	}
	return meta, nil
}

// genTrack builds one track's fixtures and returns its spec. The last
// track (when there are at least two) is corrupted at a known byte
// offset; that offset is returned, otherwise -1.
func genTrack(inputs string, params RunParams, t int) (*TrackSpec, int64, error) {
	id := fmt.Sprintf("track%d", t)
	rng := rand.New(rand.NewSource(params.Seed*7919 + int64(t)*104729))
	codecs := []string{"h264", "h265", "av1"}
	spec := &TrackSpec{ID: id, Kind: "es", Codec: codecs[t%3], TsBits: 40}
	switch t % 3 {
	case 0:
		spec.TBNum, spec.TBDen = 1, 1_000_000
	case 1:
		spec.TBNum, spec.TBDen, spec.TsBits = 1, 90_000, 32
	case 2:
		spec.TBNum, spec.TBDen, spec.TsBits = 1, 90_000, 32
	}
	if t == 2 { // remote commentary arrives as RTP
		spec.Kind = "rtp"
		spec.Codec = "av1"
	}
	// Drift scenarios: track 0 drifts 30 ppm, the RTP track 80 ppm
	// (both within the 500 ppm threshold, anchors provided); other
	// tracks provide no anchors and stay unaligned.
	switch t {
	case 0:
		spec.DriftPPM = 30
	case 2:
		spec.DriftPPM = 80
	}
	corrupt := t == params.Tracks-1 && params.Tracks >= 2

	n := params.Frames
	delta0 := spec.TBDen / (30 * spec.TBNum) // ticks per frame at 30 fps
	// Timestamps: 32-bit tracks start close to the wrap.
	ts := int64(delta0) * 10
	if spec.TsBits == 32 {
		ts = int64(1)<<32 - int64(delta0)*int64(n/2)
	}
	streamName := id + "." + spec.Codec
	if spec.Kind == "rtp" {
		streamName = id + ".rtp"
	}
	spec.Stream = streamName
	if spec.Kind == "es" {
		spec.Index = id + ".idx.mp4"
	}
	if spec.DriftPPM > 0 {
		spec.Anchors = id + ".anchors.json"
	}

	var stream, capture []byte
	var entries []bitstream.IndexEntry
	var anchors []clock.Anchor
	var offsets []int64
	// Exact drifted reference time: ref = ts * tbNs * (1e6+drift)/1e6.
	tbNs := new(big.Rat).Mul(big.NewRat(spec.TBNum, spec.TBDen), big.NewRat(1_000_000_000, 1))
	slope := new(big.Rat).Mul(tbNs, big.NewRat(1_000_000+spec.DriftPPM, 1_000_000))
	seq := uint16(65500) // RTP sequence starts near the 16-bit wrap
	for i := 0; i < n; i++ {
		if i > 0 {
			d := delta0/2 + rng.Int63n(delta0) // VFR: 0.5x..1.5x nominal
			switch {
			case i%47 == 0:
				d = 0 // duplicate timestamp
			case i%73 == 0:
				d += 4 * delta0 // gap (missing samples)
			case i%91 == 0:
				d = -delta0 / 4 // reversed timestamp
			}
			ts += d
		}
		rawTS := uint64(ts) & ((uint64(1) << spec.TsBits) - 1)
		payload := randPayload(rng, 48000+rng.Intn(48000))
		sample := encodeSample(spec.Codec, i%30 == 0, payload)
		offsets = append(offsets, int64(len(stream)))
		entries = append(entries, bitstream.IndexEntry{
			Offset: int64(len(stream)), Length: int64(len(sample)),
			Timestamp: rawTS, Keyframe: i%30 == 0,
			Digest: bitstream.DigestOf(sample),
		})
		stream = append(stream, sample...)
		if spec.Kind == "rtp" {
			pkt := rtp.Packet{Header: rtp.Header{
				Version: 2, PayloadType: 96, SequenceNumber: seq,
				Timestamp: uint32(rawTS), SSRC: 0x15535,
			}, Payload: sample}
			b, err := pkt.Marshal()
			if err != nil {
				return nil, -1, err
			}
			var rec [4]byte
			binary.BigEndian.PutUint32(rec[:], uint32(len(b)))
			capture = append(capture, rec[:]...)
			capture = append(capture, b...)
			seq++
		}
		if i%25 == 0 && spec.DriftPPM > 0 {
			ref := new(big.Rat).Mul(new(big.Rat).SetInt64(ts), slope)
			anchors = append(anchors, clock.Anchor{Local: ts, Ref: clock.RoundRat(ref)})
		}
	}

	// Corrupt the last track at a known offset: set the forbidden bit
	// of a NAL/OBU header inside the stream.
	corruptOff := int64(-1)
	if corrupt {
		k := n * 3 / 5
		hdrOff := offsets[k]
		if spec.Codec != "av1" {
			hdrOff += 3 // skip the 3-byte start code
		}
		stream[hdrOff] |= 0x80
		corruptOff = hdrOff
	}

	out := stream
	if spec.Kind == "rtp" {
		out = capture
	}
	if err := os.WriteFile(filepath.Join(inputs, streamName), out, 0o644); err != nil {
		return nil, -1, err
	}
	if spec.Index != "" {
		idx := &bitstream.Index{
			Track: id, Codec: spec.Codec,
			TimebaseNum: spec.TBNum, TimebaseDen: spec.TBDen,
			Entries: entries,
		}
		if err := bitstream.WriteIndexFile(filepath.Join(inputs, spec.Index), idx); err != nil {
			return nil, -1, err
		}
	}
	if spec.Anchors != "" {
		data, err := json.Marshal(anchors)
		if err != nil {
			return nil, -1, err
		}
		if err := os.WriteFile(filepath.Join(inputs, spec.Anchors), data, 0o644); err != nil {
			return nil, -1, err
		}
	}
	return spec, corruptOff, nil
}

// randPayload returns n deterministic bytes with no 0x000001 or
// 0x00000001 sequence (so Annex-B start codes stay unambiguous): no
// two consecutive zero bytes, and no trailing zero (which would merge
// with the next start code's leading zeros).
func randPayload(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		v := byte(rng.Intn(256))
		if v == 0 && i > 0 && b[i-1] == 0 {
			v = 0x10
		}
		b[i] = v
	}
	if n > 0 && b[n-1] == 0 {
		b[n-1] = 0x10
	}
	return b
}

// encodeSample wraps payload in one NAL unit / OBU for the codec.
func encodeSample(codec string, keyframe bool, payload []byte) []byte {
	switch codec {
	case "h264":
		hdr := byte(0x41) // non-IDR, nal_ref_idc=2
		if keyframe {
			hdr = 0x65 // IDR
		}
		return append([]byte{0, 0, 1, hdr}, payload...)
	case "h265":
		t := byte(1) // TRAIL_R
		if keyframe {
			t = 19 // IDR_W_RADL
		}
		return append([]byte{0, 0, 1, t << 1, 0x01}, payload...)
	default: // av1: OBU with leb128 size
		t := byte(6) // frame OBU
		if keyframe {
			t = 3 // frame header OBU (keyframe marker)
		}
		out := []byte{(t << 3) | 0x02}
		out = append(out, leb128(uint64(len(payload)))...)
		return append(out, payload...)
	}
}

// leb128 encodes v as unsigned leb128.
func leb128(v uint64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if v == 0 {
			return out
		}
	}
}
