// Package fixture generates deterministic MP4 files in code — no
// external assets. It also produces the four abnormal samples required
// by the acceptance flow: truncated moov, corrupt mdat, non-monotonic
// timestamps and audio-sample boundaries landing exactly on cut points.
package fixture

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// ColorSpec is nclx colour metadata for a video track.
type ColorSpec struct {
	Primaries, Transfer, Matrix uint16
	FullRange                   bool
}

// TrackSpec describes one elementary track of a generated file.
type TrackSpec struct {
	Kind          string // "video" or "audio"
	Codec         string // sample entry type: "avc1", "mp4a", anything else = unknown
	Timescale     uint32
	FrameDuration uint32 // ticks per sample
	SampleCount   int
	KeyframeEvery int // video: every Nth sample is a keyframe
	BaseSize      int // bytes per sample before Bitrate scaling
	Bitrate       int // representation bitrate multiplier
	Channels      uint16
	SampleRate    uint32
	Color         *ColorSpec
	CttsOffsets   []int32 // optional per-sample composition offsets (ctts v1)
}

// Spec describes one generated file.
type Spec struct {
	Tracks        []TrackSpec
	MdatShortfall int // bytes missing from the end of the mdat payload
	MoovTruncAt   int // if >0, file is cut this many bytes into moov
}

func u16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
func u32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func u64(v uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b }

func box(typ string, parts ...[]byte) []byte {
	var p []byte
	for _, x := range parts {
		p = append(p, x...)
	}
	out := append(u32(uint32(len(p)+8)), []byte(typ)...)
	return append(out, p...)
}

func full(typ string, ver byte, flags uint32, parts ...[]byte) []byte {
	head := append([]byte{ver}, []byte{byte(flags >> 16), byte(flags >> 8), byte(flags)}...)
	return box(typ, append(head, concat(parts)...))
}

func concat(parts [][]byte) []byte {
	var p []byte
	for _, x := range parts {
		p = append(p, x...)
	}
	return p
}

// sampleBytes returns deterministic pseudo-content for one sample.
func sampleBytes(track, index, size int) []byte {
	b := make([]byte, size)
	for j := range b {
		b[j] = byte(j + index*7 + track*13)
	}
	return b
}

func sampleSizes(t TrackSpec) []int {
	s := make([]int, t.SampleCount)
	for i := range s {
		s[i] = t.BaseSize * t.Bitrate
		if t.Kind == "video" && t.KeyframeEvery > 0 && i%t.KeyframeEvery == 0 {
			s[i] *= 3
		}
	}
	return s
}

func stsd(t TrackSpec) []byte {
	var entry []byte
	switch t.Codec {
	case "avc1":
		h := concat([][]byte{make([]byte, 6), u16(1), make([]byte, 16),
			u16(1920), u16(1080), u32(0x00480000), u32(0x00480000), u32(0),
			u16(1), make([]byte, 32), u16(0x18), u16(0xffff)})
		avcC := box("avcC", []byte{1, 0x64, 0, 0x1f, 0xff, 0xe1, 0, 4, 1, 2, 3, 4, 1, 0, 2, 5, 6})
		kids := avcC
		if t.Color != nil {
			fr := byte(0)
			if t.Color.FullRange {
				fr = 0x80
			}
			kids = append(kids, box("colr", append([]byte("nclx"),
				concat([][]byte{u16(t.Color.Primaries), u16(t.Color.Transfer), u16(t.Color.Matrix), {fr}})...))...)
		}
		entry = box("avc1", h, kids)
	case "mp4a":
		h := concat([][]byte{make([]byte, 6), u16(1), make([]byte, 8),
			u16(t.Channels), u16(16), u16(0), u16(0), u32(t.SampleRate << 16)})
		entry = box("mp4a", h)
	default: // unknown codec structure: probe must fail explicitly
		entry = box(t.Codec, make([]byte, 6), u16(1))
	}
	return full("stsd", 0, 0, u32(1), entry)
}

func stbl(t TrackSpec, sizes []int, chunkOff uint32) []byte {
	stts := full("stts", 0, 0, u32(1), u32(uint32(t.SampleCount)), u32(t.FrameDuration))
	stsc := full("stsc", 0, 0, u32(1), u32(1), u32(uint32(t.SampleCount)), u32(1))
	sz := concat([][]byte{u32(0), u32(uint32(t.SampleCount))})
	for _, s := range sizes {
		sz = append(sz, u32(uint32(s))...)
	}
	stsz := full("stsz", 0, 0, sz)
	stco := full("stco", 0, 0, u32(1), u32(chunkOff))
	kids := [][]byte{stsd(t), stts, stsc, stsz, stco}
	if t.Kind == "video" && t.KeyframeEvery > 0 {
		var ss []byte
		var n uint32
		for i := 0; i < t.SampleCount; i += t.KeyframeEvery {
			ss = append(ss, u32(uint32(i+1))...)
			n++
		}
		kids = append(kids, full("stss", 0, 0, u32(n), ss))
	}
	if len(t.CttsOffsets) > 0 {
		ct := concat([][]byte{u32(uint32(len(t.CttsOffsets)))})
		for _, o := range t.CttsOffsets {
			ct = append(ct, u32(1)...)
			ct = append(ct, u32(uint32(o))...)
		}
		kids = append(kids, full("ctts", 1, 0, ct))
	}
	return box("stbl", kids...)
}

func trak(id int, t TrackSpec, sizes []int, chunkOff uint32) []byte {
	dur := uint64(t.FrameDuration) * uint64(t.SampleCount)
	tkhd := full("tkhd", 0, 3, u32(0), u32(0), u32(uint32(id)), u32(0),
		u32(uint32(dur*1000/uint64(t.Timescale))), make([]byte, 52), u16(0), u16(0))
	mdhd := full("mdhd", 0, 0, u32(0), u32(0), u32(t.Timescale), u32(uint32(dur)), u16(0x55c4), u16(0))
	handler := "vide"
	header := "vmhd"
	if t.Kind == "audio" {
		handler, header = "soun", "smhd"
	}
	hdlr := full("hdlr", 0, 0, u32(0), []byte(handler), make([]byte, 12), []byte(t.Kind+"\x00"))
	head := full(header, 0, 1, make([]byte, 4))
	dref := full("dref", 0, 0, u32(1), full("url ", 0, 1))
	minf := box("minf", head, box("dinf", dref), stbl(t, sizes, chunkOff))
	return box("trak", tkhd, box("mdia", mdhd, hdlr, minf))
}

// Build renders the whole file deterministically.
func Build(spec Spec) []byte {
	ftyp := box("ftyp", []byte("isom"), u32(0x200), []byte("isomiso2avc1mp41"))
	sizes := make([][]int, len(spec.Tracks))
	var payload []byte
	for i, t := range spec.Tracks {
		sizes[i] = sampleSizes(t)
		for _, s := range sizes[i] {
			payload = append(payload, sampleBytes(i, len(payload), s)...)
		}
	}
	if spec.MdatShortfall > 0 {
		payload = payload[:len(payload)-spec.MdatShortfall]
	}
	mdat := box("mdat", payload)
	chunkBase := uint32(len(ftyp) + 8)
	traks := make([][]byte, 0, len(spec.Tracks))
	off := chunkBase
	var maxDurMs uint64
	for i, t := range spec.Tracks {
		traks = append(traks, trak(i+1, t, sizes[i], off))
		for _, s := range sizes[i] {
			off += uint32(s)
		}
		d := uint64(t.FrameDuration) * uint64(t.SampleCount) * 1000 / uint64(t.Timescale)
		if d > maxDurMs {
			maxDurMs = d
		}
	}
	mvhd := full("mvhd", 0, 0, u32(0), u32(0), u32(1000), u32(uint32(maxDurMs)),
		u32(0x00010000), u16(0x0100), make([]byte, 70))
	moov := box("moov", concat(append([][]byte{mvhd}, traks...)))
	out := concat([][]byte{ftyp, mdat, moov})
	if spec.MoovTruncAt > 0 {
		out = out[:len(ftyp)+len(mdat)+spec.MoovTruncAt]
	}
	return out
}

// WriteFile writes a generated fixture and returns its SHA-256.
func WriteFile(dir, name string, spec Spec) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	b := Build(spec)
	sum := sha256.Sum256(b)
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum[:]), nil
}

// Standard returns the canonical good specification shared by all
// representations: 25 fps video (90000/3600) and 48 kHz audio with
// 960-sample frames, so 2 s cut points land exactly on both grids.
func Standard(bitrate int, frames int) Spec {
	return Spec{Tracks: []TrackSpec{
		{Kind: "video", Codec: "avc1", Timescale: 90000, FrameDuration: 3600,
			SampleCount: frames, KeyframeEvery: 25, BaseSize: 1500, Bitrate: bitrate,
			Color: &ColorSpec{Primaries: 1, Transfer: 1, Matrix: 1}},
		{Kind: "audio", Codec: "mp4a", Timescale: 48000, FrameDuration: 960,
			SampleCount: frames * 2, BaseSize: 300, Bitrate: bitrate,
			Channels: 2, SampleRate: 48000},
	}}
}

// RepName is the canonical fixture file name of a representation.
func RepName(i int) string { return fmt.Sprintf("rep%d.mp4", i+1) }
