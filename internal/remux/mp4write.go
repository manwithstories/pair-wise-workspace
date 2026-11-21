package remux

import (
	"encoding/binary"

	"mediameld/internal/model"
)

// The writer emits its own minimal, structurally valid moov rather than
// reusing codec boxes from the source. This keeps the remux independent of
// codec specifics while preserving every sample byte verbatim.

func buildFtyp() []byte {
	return boxOf("ftyp", []byte("isom"), u32(0x200), []byte("isom"), []byte("iso2"), []byte("mp41"))
}

// buildMoov lays out mvhd + one trak per input track. offsets maps track id to
// the absolute output offset of each sample's payload.
func buildMoov(in Input, offsets map[string][]int64, payloadStart int64) []byte {
	var maxMs int64
	var traks [][]byte
	for _, t := range in.Tracks {
		var dur int64
		for _, s := range t.Samples {
			if s.PTS+s.Duration > dur {
				dur = s.PTS + s.Duration
			}
		}
		ms := dur * 1000 / t.Timescale
		if ms > maxMs {
			maxMs = ms
		}
		traks = append(traks, buildTrak(t, offsets[t.ID], ms))
	}
	mvhd := fullBox("mvhd", 0, 0,
		u32(0), u32(0), u32(1000), u32(uint32(maxMs)),
		u32(0x00010000), u16(0x0100), zero(2), zero(8),
		join(matrixParts(identityMatrix())...), zero(24), u32(uint32(len(in.Tracks)+1)))
	parts := [][]byte{mvhd}
	parts = append(parts, traks...)
	return boxOf("moov", parts...)
}

func buildTrak(t TrackInput, offsets []int64, movieDur int64) []byte {
	volume := uint16(0)
	if t.Kind == model.KindAudio {
		volume = 0x0100
	}
	w, h := uint32(t.Width)<<16, uint32(t.Height)<<16
	if t.Rotation == 90 || t.Rotation == 270 {
		w, h = h, w
	}
	tkhd := fullBox("tkhd", 0, 0x000007,
		u32(0), u32(0), u32(trackNumericID(t.ID)), u32(0), u32(uint32(movieDur)),
		zero(8), u16(0), u16(0), u16(volume), u16(0),
		join(matrixParts(rotationMatrix(t.Rotation))...), u32(w), u32(h))
	trackDur := int64(0)
	for _, s := range t.Samples {
		if s.PTS+s.Duration > trackDur {
			trackDur = s.PTS + s.Duration
		}
	}
	mdhd := fullBox("mdhd", 0, 0,
		u32(0), u32(0), u32(uint32(t.Timescale)), u32(uint32(trackDur)),
		u16(0x55c4), u16(0))
	hdlrType := "vide"
	if t.Kind == model.KindAudio {
		hdlrType = "soun"
	}
	hdlr := fullBox("hdlr", 0, 0, u32(0), []byte(hdlrType), zero(12), []byte("MediaMeld\x00"))
	minf := boxOf("minf", mediaHeader(t.Kind), dinfBox(), sampleTables(t, offsets))
	mdia := boxOf("mdia", mdhd, hdlr, minf)
	return boxOf("trak", tkhd, mdia)
}

func mediaHeader(kind model.TrackKind) []byte {
	if kind == model.KindAudio {
		return fullBox("smhd", 0, 0, u16(0), u16(0))
	}
	return fullBox("vmhd", 0, 1, u16(0), u16(0), u16(0), u16(0))
}

func dinfBox() []byte {
	dref := fullBox("dref", 0, 0, u32(1), fullBox("url ", 0, 1))
	return boxOf("dinf", dref)
}

func sampleTables(t TrackInput, offsets []int64) []byte {
	stsd := stsdFor(t)
	var sttsEntries [][]byte
	var count int
	for i := 0; i < len(t.Samples); {
		j := i + 1
		for j < len(t.Samples) && t.Samples[j].Duration == t.Samples[i].Duration {
			j++
		}
		sttsEntries = append(sttsEntries, u32(uint32(j-i)), u32(uint32(t.Samples[i].Duration)))
		count++
		i = j
	}
	stts := fullBox("stts", 0, 0, u32(uint32(count)), join(sttsEntries...))
	stsc := fullBox("stsc", 0, 0, u32(1), u32(1), u32(1), u32(1))
	var sizes [][]byte
	for _, s := range t.Samples {
		sizes = append(sizes, u32(uint32(s.Size)))
	}
	stsz := fullBox("stsz", 0, 0, u32(0), u32(uint32(len(t.Samples))), join(sizes...))
	var chunkOff [][]byte
	for _, o := range offsets {
		chunkOff = append(chunkOff, u32(uint32(o)))
	}
	stco := fullBox("stco", 0, 0, u32(uint32(len(offsets))), join(chunkOff...))
	parts := [][]byte{stsd, stts, stsc, stsz, stco}
	// Preserve non-zero composition offsets exactly.
	hasCTS := false
	for _, s := range t.Samples {
		if s.PTS != s.DTS {
			hasCTS = true
			break
		}
	}
	if hasCTS {
		var entries [][]byte
		for _, s := range t.Samples {
			entries = append(entries, u32(1), i32(int32(s.PTS-s.DTS)))
		}
		parts = append(parts, fullBox("ctts", 0, 0, u32(uint32(len(t.Samples))), join(entries...)))
	}
	return boxOf("stbl", parts...)
}

func stsdFor(t TrackInput) []byte {
	var entry []byte
	if t.Kind == model.KindAudio {
		entry = boxOf("mp4a", zero(6), u16(1), zero(8), u16(2), u16(16), u16(0), u16(0), u32(48000<<16))
	} else {
		entry = boxOf("avc1", zero(6), u16(1), zero(16),
			u16(uint16(t.Width)), u16(uint16(t.Height)),
			u32(72<<16), u32(72<<16), u32(0), u16(1), zero(32), u16(0x18), i16(-1))
	}
	return fullBox("stsd", 0, 0, u32(1), entry)
}

func trackNumericID(id string) uint32 {
	var v uint32
	for _, c := range id {
		if c < '0' || c > '9' {
			continue
		}
		v = v*10 + uint32(c-'0')
	}
	if v == 0 {
		v = 1
	}
	return v
}

func identityMatrix() []int32 {
	const one = int32(1) << 16
	return []int32{one, 0, 0, 0, one, 0, 0, 0, 0x40000000}
}

func rotationMatrix(deg int) []int32 {
	const one = int32(1) << 16
	switch ((deg % 360) + 360) % 360 {
	case 90:
		return []int32{0, one, 0, -one, 0, 0, 0, 0, 0x40000000}
	case 180:
		return []int32{-one, 0, 0, 0, -one, 0, 0, 0, 0x40000000}
	case 270:
		return []int32{0, -one, 0, one, 0, 0, 0, 0, 0x40000000}
	default:
		return identityMatrix()
	}
}

// --- byte helpers ---

func u16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
func u32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func i32(v int32) []byte  { return u32(uint32(v)) }
func i16(v int16) []byte  { return u16(uint16(v)) }
func zero(n int) []byte   { return make([]byte, n) }

func join(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func boxOf(typ string, parts ...[]byte) []byte {
	payload := join(parts...)
	out := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(out, uint32(8+len(payload)))
	copy(out[4:], typ)
	return append(out, payload...)
}

func fullBox(typ string, version byte, flags uint32, parts ...[]byte) []byte {
	hdr := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return boxOf(typ, append([][]byte{hdr}, parts...)...)
}

func matrixParts(m []int32) [][]byte {
	out := make([][]byte, len(m))
	for i, v := range m {
		out[i] = i32(v)
	}
	return out
}
