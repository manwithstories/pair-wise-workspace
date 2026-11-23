package gen

import (
	"encoding/binary"
	"fmt"
)

// TrackSpec describes one track in a generated representation.
type TrackSpec struct {
	TrackID       uint32
	Timescale     uint32
	Handler       string
	Codec         string
	Width         uint16
	Height        uint16
	SampleDelta   uint32
	SampleSize    uint32
	KeyframeEvery int
}

// Muxer accumulates one track's samples into a CMAF segment sequence.
type Muxer struct {
	Spec          TrackSpec
	sequence      uint32
	totalSamples  int
	DurationTicks uint64
}

// NewMuxer returns a muxer for spec starting at sequence number 1.
func NewMuxer(spec TrackSpec) *Muxer {
	if spec.Timescale == 0 {
		spec.Timescale = 90000
	}
	if spec.SampleDelta == 0 {
		spec.SampleDelta = 3000
	}
	if spec.SampleSize == 0 {
		spec.SampleSize = 512
	}
	if spec.Handler == "" {
		spec.Handler = "vide"
	}
	if spec.Codec == "" {
		spec.Codec = "avc1"
	}
	if spec.KeyframeEvery <= 0 {
		spec.KeyframeEvery = 30
	}
	return &Muxer{Spec: spec}
}

// SampleCount returns how many samples have been emitted so far.
func (m *Muxer) SampleCount() int { return m.totalSamples }

// Sequence returns the next expected mfhd sequence number.
func (m *Muxer) Sequence() uint32 { return m.sequence + 1 }

// Init builds an initialization segment with one track.
func (m *Muxer) Init() []byte {
	sp := m.Spec
	durationTicks := uint32(sp.SampleDelta) * uint32(sp.KeyframeEvery)
	mvhdTS := 1000
	durMS := uint32(uint64(durationTicks) * 1000 / uint64(sp.Timescale))

	trex := fullBox("trex", 0, 0, func() []byte {
		var p []byte
		p = u32(p, sp.TrackID)
		p = u32(p, 1)
		p = u32(p, sp.SampleDelta)
		p = u32(p, sp.SampleSize)
		p = u32(p, 0)
		return p
	}())

	stbl := box("stbl", cat(
		fullBox("stsd", 0, 0, cat(u32(nil, 1), Avc1(sp.Width, sp.Height))),
		fullBox("stts", 0, 0, cat(u32(nil, 1), u32(nil, uint32(sp.KeyframeEvery)), u32(nil, sp.SampleDelta))),
		fullBox("stsc", 0, 0, u32(nil, 0)),
		fullBox("stsz", 0, 0, cat(u32(nil, sp.SampleSize), u32(nil, 0))),
		fullBox("stco", 0, 0, u32(nil, 0)),
	))
	minf := box("minf", cat(
		fullBox("vmhd", 0, 1, cat(make([]byte, 6), u16(nil, 0), u16(nil, 0), u16(nil, 0), u16(nil, 0))),
		box("dinf", fullBox("dref", 0, 0, cat(u32(nil, 0)))),
		stbl,
	))
	mdia := box("mdia", cat(
		Mdhd(sp.Timescale, durationTicks),
		Hdlr(sp.Handler, "MediaFlow"),
		minf,
	))
	trak := box("trak", cat(Tkhd(sp.TrackID, durMS, uint32(sp.Width), uint32(sp.Height)), mdia))
	moov := box("moov", cat(
		Mvhd(uint32(mvhdTS), durMS, sp.TrackID+1),
		trak,
		box("mvex", trex),
	))
	return cat(Ftyp(), moov)
}

// Fragment describes one moof+mdat pair to emit.
type FragmentSpec struct {
	Sequence   uint32
	BaseTicks  uint64
	Samples    int
	Keyframes  []int
	TruncateTo int
	DropMfhd   bool
	Duration   uint32
	Size       uint32
}

// BuildFragment emits one moof and mdat for the muxer's track.
func (m *Muxer) BuildFragment(fs FragmentSpec) ([]byte, FragmentInfo) {
	sp := m.Spec
	if fs.Sequence == 0 {
		fs.Sequence = m.Sequence()
	}
	if fs.Duration == 0 {
		fs.Duration = sp.SampleDelta
	}
	if fs.Size == 0 {
		fs.Size = sp.SampleSize
	}
	keySet := make(map[int]bool, len(fs.Keyframes))
	for _, k := range fs.Keyframes {
		keySet[k] = true
	}

	// Layout: moof(header + mfhd + traf(tfhd,tfdt,trun)) + mdat.
	tfhdFlags := uint32(0x020000) // default-base-is-moof
	tfhd := fullBox("tfhd", 0, tfhdFlags|0x000008|0x000010|0x000020, cat(
		u32(nil, sp.TrackID), u32(nil, fs.Duration), u32(nil, fs.Size), u32(nil, 0)))
	tfdt := fullBox("tfdt", 1, 0, u64(nil, fs.BaseTicks))

	trunPayload := func() []byte {
		var p []byte
		p = u32(p, uint32(fs.Samples))
		p = u32(p, 0) // filled in below once moof size is known
		for i := 0; i < fs.Samples; i++ {
			p = u32(p, fs.Duration)
			p = u32(p, fs.Size)
			flags := uint32(0x02000000)
			if !keySet[i] {
				flags = 0x01010000
			}
			p = u32(p, flags)
		}
		return p
	}
	// trun flags: data-offset, sample-duration, sample-size, sample-flags.
	trunFlags := uint32(0x000001 | 0x000100 | 0x000200 | 0x000400)
	mfhd := fullBox("mfhd", 0, 0, u32(nil, fs.Sequence))

	// Two-pass size: compute moof with a placeholder data offset.
	build := func(dataOffset int32) []byte {
		p := trunPayload()
		binary.BigEndian.PutUint32(p[4:8], uint32(dataOffset))
		trun := fullBox("trun", 0, trunFlags, p)
		traf := box("traf", cat(tfhd, tfdt, trun))
		parts := [][]byte{traf}
		if !fs.DropMfhd {
			parts = append([][]byte{mfhd}, parts...)
		}
		return box("moof", cat(parts...))
	}
	moof := build(0)
	dataOffset := int32(len(moof) + 8) // mdat header
	moof = build(dataOffset)

	payload := make([]byte, fs.Samples*int(fs.Size))
	// Fill with a deterministic pattern so byte-level damage is visible.
	for i := range payload {
		payload[i] = byte(i*31 + int(fs.Sequence))
	}
	mdat := box("mdat", payload)
	seg := cat(Styp(), moof, mdat)
	if fs.TruncateTo > 0 && fs.TruncateTo < len(seg) {
		seg = seg[:fs.TruncateTo]
	}

	m.sequence = fs.Sequence
	m.totalSamples += fs.Samples
	m.DurationTicks = fs.BaseTicks + uint64(fs.Duration)*uint64(fs.Samples)

	info := FragmentInfo{
		Sequence:   fs.Sequence,
		BaseTicks:  fs.BaseTicks,
		Samples:    fs.Samples,
		MoofOffset: len(Styp()),
		Truncated:  fs.TruncateTo > 0 && fs.TruncateTo < len(cat(Styp(), moof, mdat)),
	}
	return seg, info
}

// FragmentInfo is metadata for one emitted fragment, used by tests and the
// demo to know the ground truth before parsing.
type FragmentInfo struct {
	Sequence   uint32
	BaseTicks  uint64
	Samples    int
	MoofOffset int
	Truncated  bool
	Keyframes  []int
}

func (fi FragmentInfo) String() string {
	return fmt.Sprintf("seq=%d tfdt=%d samples=%d moof@%d", fi.Sequence, fi.BaseTicks, fi.Samples, fi.MoofOffset)
}
