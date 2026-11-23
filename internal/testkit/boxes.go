// Package testkit self-generates normal and corrupted CMAF/fMP4 fixtures.
// Nothing is read from external data files.
package testkit

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/abema/go-mp4"
)

func u32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }

// box wraps payload in a 32-bit-sized box header.
func box(typ string, payloads ...[]byte) []byte {
	body := bytes.Join(payloads, nil)
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b, uint32(8+len(body)))
	copy(b[4:], typ)
	return append(b, body...)
}

func fullBox(version byte, flags uint32, payloads ...[]byte) []byte {
	h := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return append(h, bytes.Join(payloads, nil)...)
}

func marshalBox(b mp4.IImmutableBox) []byte {
	var buf bytes.Buffer
	if _, err := mp4.Marshal(&buf, b, mp4.Context{}); err != nil {
		panic(fmt.Sprintf("marshal %s: %v", b.GetType().String(), err))
	}
	return buf.Bytes()
}

// InitSpec describes one initialization segment with a single track.
type InitSpec struct {
	TrackID   uint32
	Timescale uint32
	Handler   string // "vide" or "soun"
	Width     uint32
	Height    uint32
}

func buildInit(spec InitSpec) []byte {
	ftyp := box("ftyp", []byte("iso6"), u32(0), []byte("iso6"), []byte("cmfc"), []byte("dash"))

	mvhd := &mp4.Mvhd{Timescale: 1000, DurationV0: 0, Rate: 0x00010000, Volume: 0x0100, NextTrackID: spec.TrackID + 1}
	mvhd.Matrix = [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}

	tkhd := &mp4.Tkhd{TrackID: spec.TrackID, Width: spec.Width << 16, Height: spec.Height << 16}
	tkhd.Matrix = [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
	if spec.Handler == "soun" {
		tkhd.Volume = 0x0100
	}

	mdhd := &mp4.Mdhd{Timescale: spec.Timescale, DurationV0: 0, Language: [3]byte{'u', 'n', 'd'}}

	hdlr := &mp4.Hdlr{HandlerType: [4]byte{spec.Handler[0], spec.Handler[1], spec.Handler[2], spec.Handler[3]}, Name: "archivecheck"}
	var mhd mp4.IImmutableBox = &mp4.Vmhd{}
	if spec.Handler == "soun" {
		mhd = &mp4.Smhd{}
	}

	// A minimal, self-contained data reference and empty sample description are
	// sufficient for an archive index; no decoder ever inspects the samples.
	dref := box("dref", fullBox(0, 0, u32(1), box("url ", fullBox(0, 1))))
	dinf := box("dinf", dref)
	stsd := box("stsd", fullBox(0, 0, u32(0)))
	stbl := box("stbl", stsd)
	minf := box("minf", box(mhd.GetType().String(), marshalBox(mhd)), dinf, stbl)
	mdia := box("mdia", box("mdhd", marshalBox(mdhd)), box("hdlr", marshalBox(hdlr)), minf)
	trak := box("trak", box("tkhd", marshalBox(tkhd)), mdia)

	trex := &mp4.Trex{TrackID: spec.TrackID, DefaultSampleDescriptionIndex: 1}
	mvex := box("mvex", box("trex", marshalBox(trex)))
	moov := box("moov", box("mvhd", marshalBox(mvhd)), trak, mvex)

	return append(ftyp, moov...)
}

// SampleSpec is one sample in a generated media segment.
type SampleSpec struct {
	Duration uint32
	Size     uint32
	Sync     bool
	CTO      int32
}

// FragmentSpec controls one generated media segment.
type FragmentSpec struct {
	Seq        uint32
	OmitMfhd   bool
	TrackID    uint32
	BaseTime   uint64
	Samples    []SampleSpec
	TruncateAt int  // if >0, emit an mdat truncated to this payload length
	BadMdatLen bool // if true, declare an mdat size that overflows the file
}

func buildFragment(spec FragmentSpec) []byte {
	styp := box("styp", []byte("msdh"), u32(0), []byte("msdh"), []byte("cmfs"))
	samples := bytes.Join(buildSampleBytes(spec.Samples), nil)
	mdatPayloadLen := len(samples)
	if spec.TruncateAt > 0 {
		mdatPayloadLen = spec.TruncateAt
	}

	build := func(dataOffset int32) []byte {
		var moofParts [][]byte
		if !spec.OmitMfhd {
			mfhd := &mp4.Mfhd{SequenceNumber: spec.Seq}
			moofParts = append(moofParts, box("mfhd", marshalBox(mfhd)))
		}
		tfhd := &mp4.Tfhd{TrackID: spec.TrackID}
		tfhd.AddFlag(mp4.TfhdDefaultBaseIsMoof)
		tfdt := &mp4.Tfdt{BaseMediaDecodeTimeV1: spec.BaseTime}
		tfdt.SetVersion(1)

		var entries []mp4.TrunEntry
		for _, s := range spec.Samples {
			flags := uint32(0x02000000)
			if !s.Sync {
				flags = 0x01010000
			}
			entries = append(entries, mp4.TrunEntry{
				SampleDuration:                s.Duration,
				SampleSize:                    s.Size,
				SampleFlags:                   flags,
				SampleCompositionTimeOffsetV0: uint32(s.CTO),
			})
		}
		trun := &mp4.Trun{SampleCount: uint32(len(entries)), Entries: entries}
		trun.AddFlag(0x000001 | 0x000004 | 0x000100 | 0x000200 | 0x000400 | 0x000800)
		trun.DataOffset = dataOffset
		first := uint32(0x02000000)
		if len(spec.Samples) > 0 && !spec.Samples[0].Sync {
			first = 0x01010000
		}
		trun.FirstSampleFlags = first
		traf := box("traf", box("tfhd", marshalBox(tfhd)), box("tfdt", marshalBox(tfdt)), box("trun", marshalBox(trun)))
		moofParts = append(moofParts, traf)
		return box("moof", moofParts...)
	}

	moof := build(0)
	dataOffset := int32(len(moof) + 8)
	moof = build(dataOffset)

	mdatSize := 8 + mdatPayloadLen
	var mdat []byte
	if spec.BadMdatLen {
		hdr := make([]byte, 8)
		// Declare a size far past EOF to trigger an out-of-bounds fault.
		binary.BigEndian.PutUint32(hdr, uint32(0x7fffffff))
		copy(hdr[4:], "mdat")
		mdat = append(hdr, samples[:min(len(samples), 32)]...)
	} else {
		hdr := make([]byte, 8)
		binary.BigEndian.PutUint32(hdr, uint32(mdatSize))
		copy(hdr[4:], "mdat")
		payload := samples
		if spec.TruncateAt > 0 {
			payload = samples[:min(len(samples), spec.TruncateAt)]
		}
		mdat = append(hdr, payload...)
	}
	return bytes.Join([][]byte{styp, moof, mdat}, nil)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func buildSampleBytes(samples []SampleSpec) [][]byte {
	out := make([][]byte, len(samples))
	for i, s := range samples {
		b := make([]byte, s.Size)
		for j := range b {
			b[j] = byte(i*31 + j*7 + 3)
		}
		out[i] = b
	}
	return out
}
