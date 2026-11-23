package fixture

import (
	"bytes"
	"encoding/binary"

	"mediacore/internal/sample"
)

type genTrack struct {
	spec    TrackSpec
	id      uint32
	truth   TrackTruth
	payload [][]byte // per-sample compressed bytes
}

// Build renders the spec into a complete artifact.
func Build(s Spec) (*Artifact, error) {
	rng := newRNG(s.Seed)
	gts := make([]genTrack, len(s.Tracks))
	for i, ts := range s.Tracks {
		gt := genTrack{spec: ts, id: uint32(i + 1)}
		gt.truth = TrackTruth{ID: gt.id, Codec: ts.Codec, Timescale: ts.Timescale, SampleCount: ts.Samples}
		for k := 0; k < ts.Samples; k++ {
			gt.payload = append(gt.payload, payload(rng, ts.Codec, k))
			gt.truth.SampleSizes = append(gt.truth.SampleSizes, len(gt.payload[k]))
			gt.truth.Deltas = append(gt.truth.Deltas, ts.Delta)
		}
		gts[i] = gt
	}

	// Lay mdat out: one chunk per track, each chunk = contiguous frames.
	mdat := newBuffer()
	for i := range gts {
		gt := &gts[i]
		for _, data := range gt.payload {
			off := int64(0) // patched once mdat absolute offset is known
			gt.truth.SampleOffsets = append(gt.truth.SampleOffsets, off)
			raw(mdat, sample.Frame(gt.id, data))
		}
	}
	mdatBytes := mdat.Bytes()

	brand := "isom"
	if s.Variant == 1 {
		brand = "iso5"
	}
	art := &Artifact{Variant: s.Variant, CorruptOffset: -1, OriginalLength: -1}

	var out bytes.Buffer
	ftyp := ftypBox(brand)
	moovSize := moovSize(gts, s.Variant, s.Mode)
	mdatHeader := make([]byte, 8)
	binary.BigEndian.PutUint32(mdatHeader[:4], uint32(8+len(mdatBytes)))
	copy(mdatHeader[4:8], "mdat")

	var mdatAbs, moovAbs int64
	if s.Variant == 0 {
		moovAbs = int64(len(ftyp))
		mdatAbs = moovAbs + int64(moovSize)
		out.Write(ftyp)
		out.Write(buildMoov(gts, s, mdatAbs+8))
		out.Write(mdatHeader)
		out.Write(mdatBytes)
	} else {
		mdatAbs = int64(len(ftyp))
		moovAbs = mdatAbs + int64(8+len(mdatBytes))
		out.Write(ftyp)
		out.Write(mdatHeader)
		out.Write(mdatBytes)
		out.Write(buildMoov(gts, s, mdatAbs+8))
	}
	art.MdatOffset = mdatAbs + 8
	art.Tracks = make([]TrackTruth, len(gts))
	for i := range gts {
		// Patch absolute sample offsets: chunk start + within-mdat frame pos.
		pos := art.MdatOffset
		for j := range gts {
			gt := &gts[j]
			for k := range gt.payload {
				gt.truth.SampleOffsets[k] = pos
				pos += int64(sample.FrameHeaderLen + len(gt.payload[k]))
			}
		}
		art.Tracks[i] = gts[i].truth
	}

	b := out.Bytes()
	b, art = applyMode(b, s, art, gts)
	art.Bytes = b
	return art, nil
}

func ftypBox(brand string) []byte {
	buf := newBuffer()
	payload := newBuffer()
	s4(payload, brand)
	u32(payload, 0) // minor version
	s4(payload, brand)
	s4(payload, "mp41")
	u32(buf, uint32(8+len(payload.Bytes())))
	s4(buf, "ftyp")
	raw(buf, payload.Bytes())
	return buf.Bytes()
}

// applyMode mutates the rendered bytes for the requested damage mode.
func applyMode(b []byte, s Spec, art *Artifact, gts []genTrack) ([]byte, *Artifact) {
	switch s.Mode {
	case ModeTruncatedTail:
		art.OriginalLength = int64(len(b))
		// Cut the final sample's frame in half (at least header remains).
		last := gts[len(gts)-1].truth
		off := last.SampleOffsets[len(last.SampleOffsets)-1]
		cut := off + int64(sample.FrameHeaderLen) + int64(last.SampleSizes[len(last.SampleSizes)-1])/2
		return b[:cut], art
	case ModeCorruptSample:
		idx := s.CorruptIndex
		// Damage the first track's chosen sample payload (deterministic).
		off := gts[0].truth.SampleOffsets[idx] + int64(sample.FrameHeaderLen)
		b[off] ^= 0xFF
		art.CorruptOffset = off
		return b, art
	case ModeIndexOutOfRange:
		// The stco/co64 writer uses the out-of-range marker when mode is set.
		return b, art
	case ModeTruncatedHeader:
		art.OriginalLength = int64(len(b))
		return b[:len(b)-4], art // trim final box to a half header
	default:
		return b, art
	}
}
