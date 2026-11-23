// Package fixture generates deterministic MP4 assets (and controlled
// corruptions) entirely in-process, so tests and the demo CLI never depend
// on external media files. Boxes are written by hand because the go-mp4
// marshaller refuses to emit the inconsistent tables (truncated counts,
// zero deltas, out-of-range offsets) that the fault fixtures require.
package fixture

import (
	"bytes"
	"encoding/binary"
	"os"

	"archivecheck/internal/clock"
)

// Color carries nclx colour metadata for a generated video track.
type Color struct {
	Primaries uint16
	Transfer  uint16
	Matrix    uint16
	FullRange bool
}

// Well-known colour tuples used by the demo representations.
var (
	ColorBT709 = Color{Primaries: 1, Transfer: 1, Matrix: 1}
	ColorBT601 = Color{Primaries: 6, Transfer: 6, Matrix: 6}
)

// TrackSpec describes one generated track.
type TrackSpec struct {
	Handler     string // "vide" or "soun"
	Timescale   uint32
	SampleCount uint32
	SampleDelta uint32 // media units per sample

	KeyframeEvery int // video: every Nth sample is a sync sample; 0 = all
	Channels      int // audio
	SampleRate    int // audio
	Width, Height int // video
	Color         *Color

	Edits         []clock.Edit // elst entries; nil = none
	MediaDuration uint64       // mdhd duration override; 0 = SampleCount*SampleDelta

	// Corruption injections (zero value = clean file):
	TruncateStszTo  int   // >0: stsz declares only this many samples
	ZeroDeltaAt     int   // >0: sample at this 1-based index gets delta 0
	ChunkOffsetPlus int64 // >0: add this to the stco entry (out-of-range)
}

// Spec describes one generated file.
type Spec struct {
	MovieTimescale uint32
	Tracks         []TrackSpec
}

// Generate writes spec as a deterministic MP4 file at path.
func Generate(path string, spec Spec) error {
	if spec.MovieTimescale == 0 {
		spec.MovieTimescale = 1000
	}
	var out []byte
	out = append(out, box("ftyp", []byte("isom\x00\x00\x02\x00isomiso2"))...)
	var moov []byte
	moov = append(moov, mvhd(spec)...)
	for i, ts := range spec.Tracks {
		moov = append(moov, trak(uint32(i+1), spec.MovieTimescale, ts)...)
	}
	moovBox := box("moov", moov)
	dataStart := int64(len(out)+len(moovBox)) + 8 // first byte after mdat header
	// Patch each track's single stco entry (boxes appear in track order).
	search := moovBox
	base := 0
	for i, ts := range spec.Tracks {
		idx := bytes.Index(search, []byte("stco"))
		if idx < 0 {
			return os.ErrInvalid
		}
		slot := base + idx + 4 + 4 + 4 // type + version/flags + entry_count
		off := dataStart + int64(i)*512 + ts.ChunkOffsetPlus
		binary.BigEndian.PutUint32(moovBox[slot:], uint32(off))
		base = slot + 4
		search = moovBox[base:]
	}
	out = append(out, moovBox...)
	out = append(out, box("mdat", make([]byte, 4096))...)
	return os.WriteFile(path, out, 0o644)
}

func box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	copy(b[4:], typ)
	copy(b[8:], payload)
	return b
}

func fullBox(typ string, version byte, flags uint32, payload []byte) []byte {
	h := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return box(typ, append(h, payload...))
}

func u16(v uint16) []byte { var b [2]byte; binary.BigEndian.PutUint16(b[:], v); return b[:] }
func u32(v uint32) []byte { var b [4]byte; binary.BigEndian.PutUint32(b[:], v); return b[:] }

func unityMatrix() []byte {
	return []byte{0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0x40, 0, 0, 0}
}

func mvhd(spec Spec) []byte {
	p := append([]byte{}, u32(0)...) // creation+modification zeroed: deterministic
	p = append(p, u32(0)...)
	p = append(p, u32(spec.MovieTimescale)...)
	var dur uint64
	for _, t := range spec.Tracks {
		d := t.MediaDuration
		if d == 0 {
			d = uint64(t.SampleCount) * uint64(t.SampleDelta)
		}
		if md := d * uint64(spec.MovieTimescale) / uint64(t.Timescale); md > dur {
			dur = md
		}
	}
	p = append(p, u32(uint32(dur))...)
	p = append(p, u32(0x00010000)...)  // rate 1.0
	p = append(p, u16(0x0100)...)      // volume 1.0
	p = append(p, make([]byte, 10)...) // reserved
	p = append(p, unityMatrix()...)
	p = append(p, make([]byte, 24)...) // pre_defined
	p = append(p, u32(uint32(len(spec.Tracks))+1)...)
	return fullBox("mvhd", 0, 0, p)
}

func trak(id uint32, movieTS uint32, ts TrackSpec) []byte {
	var t []byte
	mediaDur := ts.MediaDuration
	if mediaDur == 0 {
		mediaDur = uint64(ts.SampleCount) * uint64(ts.SampleDelta)
	}
	tkhdDur := mediaDur * uint64(movieTS) / uint64(ts.Timescale)

	p := append([]byte{}, u32(0)...)
	p = append(p, u32(0)...)
	p = append(p, u32(id)...)
	p = append(p, u32(0)...)
	p = append(p, u32(uint32(tkhdDur))...)
	p = append(p, make([]byte, 8)...)
	p = append(p, u16(0)...) // layer
	p = append(p, u16(0)...) // alternate_group
	vol := uint16(0)
	if ts.Handler == "soun" {
		vol = 0x0100
	}
	p = append(p, u16(vol)...)
	p = append(p, u16(0)...)
	p = append(p, unityMatrix()...)
	p = append(p, u32(uint32(ts.Width)<<16)...)
	p = append(p, u32(uint32(ts.Height)<<16)...)
	t = append(t, fullBox("tkhd", 0, 3, p)...)

	if len(ts.Edits) > 0 {
		var e []byte
		e = append(e, u32(uint32(len(ts.Edits)))...)
		for _, ed := range ts.Edits {
			e = append(e, u32(uint32(ed.Duration))...)
			e = append(e, u32(uint32(int32(ed.MediaTime)))...)
			e = append(e, u16(1)...) // media_rate_integer
			e = append(e, u16(0)...)
		}
		t = append(t, box("edts", fullBox("elst", 0, 0, e))...)
	}
	t = append(t, mdia(ts, mediaDur)...)
	return box("trak", t)
}

func mdia(ts TrackSpec, mediaDur uint64) []byte {
	var m []byte
	if mediaDur > 0xFFFFFFFF {
		// version-1 mdhd: 64-bit creation/modification/duration
		p := append([]byte{}, make([]byte, 16)...)
		p = append(p, u32(ts.Timescale)...)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], mediaDur)
		p = append(p, b[:]...)
		p = append(p, u16(0x55c4)...)
		p = append(p, u16(0)...)
		m = append(m, fullBox("mdhd", 1, 0, p)...)
	} else {
		p := append([]byte{}, u32(0)...)
		p = append(p, u32(0)...)
		p = append(p, u32(ts.Timescale)...)
		p = append(p, u32(uint32(mediaDur))...)
		p = append(p, u16(0x55c4)...) // language: und
		p = append(p, u16(0)...)
		m = append(m, fullBox("mdhd", 0, 0, p)...)
	}

	h := append([]byte{}, u32(0)...)
	h = append(h, []byte(ts.Handler)...)
	h = append(h, make([]byte, 12)...)
	h = append(h, 0) // empty name
	m = append(m, fullBox("hdlr", 0, 0, h)...)

	var minf []byte
	if ts.Handler == "vide" {
		minf = append(minf, fullBox("vmhd", 0, 1, make([]byte, 8))...)
	} else {
		minf = append(minf, fullBox("smhd", 0, 0, make([]byte, 4))...)
	}
	minf = append(minf, stbl(ts)...)
	return box("mdia", append(m, box("minf", minf)...))
}

func stbl(ts TrackSpec) []byte {
	var s []byte
	var entry []byte
	if ts.Handler == "vide" {
		e := append([]byte{}, make([]byte, 6)...)
		e = append(e, u16(1)...)           // data_reference_index
		e = append(e, make([]byte, 16)...) // pre_defined/reserved
		e = append(e, u16(uint16(ts.Width))...)
		e = append(e, u16(uint16(ts.Height))...)
		e = append(e, u32(0x00480000)...) // horizresolution 72 dpi
		e = append(e, u32(0x00480000)...)
		e = append(e, u32(0)...)
		e = append(e, u16(1)...) // frame_count
		e = append(e, make([]byte, 32)...)
		e = append(e, u16(0x0018)...) // depth
		e = append(e, u16(0xffff)...)
		if ts.Color != nil {
			c := append([]byte{}, []byte("nclx")...)
			c = append(c, u16(ts.Color.Primaries)...)
			c = append(c, u16(ts.Color.Transfer)...)
			c = append(c, u16(ts.Color.Matrix)...)
			fr := byte(0)
			if ts.Color.FullRange {
				fr = 0x80
			}
			c = append(c, fr)
			e = append(e, box("colr", c)...)
		}
		entry = box("avc1", e)
	} else {
		e := append([]byte{}, make([]byte, 6)...)
		e = append(e, u16(1)...)
		e = append(e, u16(0)...) // entry_version
		e = append(e, make([]byte, 6)...)
		e = append(e, u16(uint16(ts.Channels))...)
		e = append(e, u16(16)...) // sample size
		e = append(e, u16(0)...)
		e = append(e, u16(0)...)
		e = append(e, u32(uint32(ts.SampleRate)<<16)...)
		entry = box("mp4a", e)
	}
	sd := append([]byte{}, u32(1)...)
	sd = append(sd, entry...)
	s = append(s, fullBox("stsd", 0, 0, sd)...)

	// stts, optionally injecting a zero-delta sample
	var st []byte
	if ts.ZeroDeltaAt > 0 {
		k := uint32(ts.ZeroDeltaAt)
		st = append(st, u32(3)...)
		st = append(st, u32(k-1)...)
		st = append(st, u32(ts.SampleDelta)...)
		st = append(st, u32(1)...)
		st = append(st, u32(0)...)
		st = append(st, u32(ts.SampleCount-k)...)
		st = append(st, u32(ts.SampleDelta)...)
	} else {
		st = append(st, u32(1)...)
		st = append(st, u32(ts.SampleCount)...)
		st = append(st, u32(ts.SampleDelta)...)
	}
	s = append(s, fullBox("stts", 0, 0, st)...)

	if ts.Handler == "vide" && ts.KeyframeEvery > 0 {
		var ss []byte
		var nums []uint32
		for n := uint32(1); n <= ts.SampleCount; n += uint32(ts.KeyframeEvery) {
			nums = append(nums, n)
		}
		ss = append(ss, u32(uint32(len(nums)))...)
		for _, n := range nums {
			ss = append(ss, u32(n)...)
		}
		s = append(s, fullBox("stss", 0, 0, ss)...)
	}

	sc := append([]byte{}, u32(1)...)
	sc = append(sc, u32(1)...)              // first_chunk
	sc = append(sc, u32(ts.SampleCount)...) // samples_per_chunk
	sc = append(sc, u32(1)...)              // sample_description_index
	s = append(s, fullBox("stsc", 0, 0, sc)...)

	count := ts.SampleCount
	if ts.TruncateStszTo > 0 {
		count = uint32(ts.TruncateStszTo)
	}
	sz := append([]byte{}, u32(100)...) // constant sample_size
	sz = append(sz, u32(count)...)
	s = append(s, fullBox("stsz", 0, 0, sz)...)

	co := append([]byte{}, u32(1)...)
	co = append(co, u32(0)...) // placeholder, patched by Generate
	s = append(s, fullBox("stco", 0, 0, co)...)
	return s
}
