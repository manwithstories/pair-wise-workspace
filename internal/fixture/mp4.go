// Package fixture synthesizes deterministic sample inputs (MP4 files,
// RTP streams, PCM, manifests, color sets) so the QC pipeline can be
// exercised without external media.
package fixture

import "encoding/binary"

// MP4Track describes one synthetic track for BuildMP4.
type MP4Track struct {
	ID           uint32
	Kind         string // "video" or "audio"
	Timescale    uint32
	Deltas       []uint32 // stts runs: sample delta per run
	Counts       []uint32 // stts runs: sample count per run
	Keyframes    []uint32 // stss sample numbers; nil = no stss box
	SampleSize   uint32   // constant sample size
	ChunkSamples uint32   // samples per chunk
	MediaTime    int64    // elst media_time; negative = no elst box
}

// MP4 is a synthetic in-memory MP4 file.
type MP4 struct {
	Data      []byte
	DataStart []int // byte offset of each track's first mdat byte
}

func box(typ string, payload []byte) []byte {
	b := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(8+len(payload)))
	copy(b[4:], typ)
	return append(b, payload...)
}

func fullbox(typ string, version byte, flags uint32, payload []byte) []byte {
	p := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return box(typ, append(p, payload...))
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func u16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

func cat(parts ...[]byte) []byte {
	var n int
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

var unitMatrix = func() []byte {
	m := make([]byte, 0, 36)
	for _, v := range []uint32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000} {
		m = append(m, u32(v)...)
	}
	return m
}()

func (t MP4Track) samples() uint64 {
	var n uint64
	for _, c := range t.Counts {
		n += uint64(c)
	}
	return n
}

func (t MP4Track) durationTicks() uint64 {
	var d uint64
	for i, c := range t.Counts {
		d += uint64(c) * uint64(t.Deltas[i])
	}
	return d
}

func stblBox(t MP4Track, offsets []uint32) []byte {
	stsd := fullbox("stsd", 0, 0, u32(0))
	stts := fullbox("stts", 0, 0, func() []byte {
		b := u32(uint32(len(t.Deltas)))
		for i, d := range t.Deltas {
			b = append(b, cat(u32(t.Counts[i]), u32(d))...)
		}
		return b
	}())
	var stss []byte
	if t.Keyframes != nil {
		b := u32(uint32(len(t.Keyframes)))
		for _, k := range t.Keyframes {
			b = append(b, u32(k)...)
		}
		stss = fullbox("stss", 0, 0, b)
	}
	stsc := fullbox("stsc", 0, 0, cat(u32(1), u32(1), u32(t.ChunkSamples), u32(1)))
	stsz := fullbox("stsz", 0, 0, cat(u32(t.SampleSize), u32(uint32(t.samples()))))
	stco := fullbox("stco", 0, 0, func() []byte {
		b := u32(uint32(len(offsets)))
		for _, o := range offsets {
			b = append(b, u32(o)...)
		}
		return b
	}())
	return box("stbl", cat(stsd, stts, stss, stsc, stsz, stco))
}

func trakBox(t MP4Track, offsets []uint32, movieDur uint32) []byte {
	tkhd := fullbox("tkhd", 0, 7, cat(
		u32(0), u32(0), u32(t.ID), u32(0), u32(movieDur),
		make([]byte, 8), u16(0), u16(0), u16(0), u16(0),
		unitMatrix, u32(0), u32(0)))
	mdhd := fullbox("mdhd", 0, 0, cat(
		u32(0), u32(0), u32(t.Timescale), u32(uint32(t.durationTicks())),
		u16(0x55c4), u16(0))) // language "und"
	handler := "vide"
	xmhd := fullbox("vmhd", 0, 1, make([]byte, 8))
	if t.Kind == "audio" {
		handler = "soun"
		xmhd = fullbox("smhd", 0, 0, make([]byte, 4))
	}
	hdlr := fullbox("hdlr", 0, 0, cat(
		u32(0), []byte(handler), make([]byte, 12), []byte("fieldqc\x00")))
	dinf := box("dinf", fullbox("dref", 0, 0, cat(u32(1), fullbox("url ", 0, 1, nil))))
	minf := box("minf", cat(xmhd, dinf, stblBox(t, offsets)))
	mdia := box("mdia", cat(mdhd, hdlr, minf))
	var edts []byte
	if t.MediaTime >= 0 {
		elst := fullbox("elst", 0, 0, cat(
			u32(1), u32(movieDur), u32(uint32(t.MediaTime)), u16(1), u16(0)))
		edts = box("edts", elst)
	}
	return box("trak", cat(tkhd, edts, mdia))
}

// BuildMP4 assembles a minimal MP4 with the given tracks. Chunk data is
// laid out track by track inside one mdat; DataStart records where each
// track's media begins so tests can truncate inside a chosen region.
func BuildMP4(tracks []MP4Track) *MP4 {
	ftyp := box("ftyp", cat([]byte("isom"), u32(0x200), []byte("isomiso2")))
	var movieDur uint32
	for _, t := range tracks {
		d := t.durationTicks() * 1000 / uint64(t.Timescale)
		if uint32(d) > movieDur {
			movieDur = uint32(d)
		}
	}
	chunks := func(t MP4Track) int {
		n := t.samples()
		return int((n + uint64(t.ChunkSamples) - 1) / uint64(t.ChunkSamples))
	}
	buildMoov := func(offsets [][]uint32) []byte {
		mvhd := fullbox("mvhd", 0, 0, cat(
			u32(0), u32(0), u32(1000), u32(movieDur),
			u32(0x10000), u16(0x100), u16(0), make([]byte, 8),
			unitMatrix, make([]byte, 24), u32(uint32(len(tracks)+1))))
		var traks []byte
		for i, t := range tracks {
			traks = append(traks, trakBox(t, offsets[i], movieDur)...)
		}
		return box("moov", cat(mvhd, traks))
	}
	zeroOffsets := make([][]uint32, len(tracks))
	for i, t := range tracks {
		zeroOffsets[i] = make([]uint32, chunks(t))
	}
	mdatOff := len(ftyp) + len(buildMoov(zeroOffsets)) + 8
	res := &MP4{DataStart: make([]int, len(tracks))}
	offsets := make([][]uint32, len(tracks))
	pos := mdatOff
	for i, t := range tracks {
		res.DataStart[i] = pos
		offsets[i] = make([]uint32, chunks(t))
		for c := range offsets[i] {
			offsets[i][c] = uint32(pos)
			n := uint64(t.ChunkSamples)
			if left := t.samples() - uint64(c)*n; left < n {
				n = left
			}
			pos += int(n * uint64(t.SampleSize))
		}
	}
	moov := buildMoov(offsets)
	mdat := box("mdat", make([]byte, pos-mdatOff))
	res.Data = cat(ftyp, moov, mdat)
	return res
}
