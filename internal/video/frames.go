// Package video reconstructs frame boundaries from recovered RTP packets,
// keeping separate decode (DTS) and presentation (PTS) timelines so variable
// frame rates and presentation-order reordering stay explicit.
package video

import (
	"encoding/binary"
	"fmt"
)

// ClockRate is the synthetic video RTP clock (90 kHz, per RFC 6184 practice).
const ClockRate = 90000

// Payload header layout for the synthetic capture:
//
//	[0:2] frame ID (uint16 BE)
//	[2]   flags: bit0 keyframe, bit1 corrupted frame
//	[3:7] composition time offset in clock units (uint32 BE)
//	[7:]  filler bytes
const (
	headerLen    = 7
	flagKeyframe = 0x01
	flagBad      = 0x02
)

// PacketInput is the pipeline-neutral view of one ordered RTP packet.
type PacketInput struct {
	Tick      int64
	SSRC      uint32
	Seq       uint16
	Timestamp uint32
	Marker    bool
	Payload   []byte
}

// Frame is one decoded-access-unit boundary recovered from packet runs.
type Frame struct {
	ID        uint32
	SSRC      uint32
	DTSUs     int64 // decode time, microseconds on the media axis
	PTSUs     int64 // presentation time, microseconds
	Keyframe  bool
	Bad       bool
	FirstTick int64 // tick of the first packet carrying this frame
	Packets   []uint16
}

// Builder accumulates packets per SSRC and emits frames at marker boundaries.
// Memory is bounded: only the frame currently in flight is held.
type Builder struct {
	inFlight map[uint32]*Frame
	curTS    map[uint32]uint32
}

// NewBuilder creates an empty frame builder.
func NewBuilder() *Builder {
	return &Builder{inFlight: map[uint32]*Frame{}, curTS: map[uint32]uint32{}}
}

// Push folds one packet into its frame. It returns a completed frame when the
// packet carries the RTP marker, otherwise nil. A bad frame is still emitted
// (with Bad set) so downstream stages can isolate it without losing the
// following keyframes.
func (b *Builder) Push(in PacketInput) (*Frame, error) {
	if len(in.Payload) < headerLen {
		return nil, fmt.Errorf("video: %d-byte payload shorter than header", len(in.Payload))
	}
	id := uint32(binary.BigEndian.Uint16(in.Payload[0:2]))
	flags := in.Payload[2]
	cto := binary.BigEndian.Uint32(in.Payload[3:7])
	f, ok := b.inFlight[in.SSRC]
	if !ok {
		f = &Frame{ID: id, SSRC: in.SSRC, FirstTick: in.Tick}
		b.inFlight[in.SSRC] = f
		b.curTS[in.SSRC] = in.Timestamp
	}
	if f.ID != id {
		return nil, fmt.Errorf("video: frame ID changed without marker ssrc=%d got=%d want=%d tick=%d",
			in.SSRC, id, f.ID, in.Tick)
	}
	f.Packets = append(f.Packets, in.Seq)
	if flags&flagBad != 0 {
		f.Bad = true
	}
	if flags&flagKeyframe != 0 {
		f.Keyframe = true
	}
	if !in.Marker {
		return nil, nil
	}
	dts := b.curTS[in.SSRC]
	f.DTSUs = int64(float64(dts) / ClockRate * 1e6)
	f.PTSUs = int64(float64(uint64(dts)+uint64(cto)) / ClockRate * 1e6)
	delete(b.inFlight, in.SSRC)
	delete(b.curTS, in.SSRC)
	return f, nil
}

// Axis is one time-ordered view over recovered frames.
type Axis struct {
	Name   string
	Frames []*Frame
}

// Axes builds the decode and presentation axes. The DTS axis follows packet
// (decode) order; the PTS axis is sorted by presentation time, which lets B
// frames and VFR cadence diverge without mutating frame identity.
func Axes(frames []*Frame) (dts Axis, pts Axis) {
	dts = Axis{Name: "dts", Frames: append([]*Frame(nil), frames...)}
	pres := append([]*Frame(nil), frames...)
	sortFrames(pres, func(a, c *Frame) bool {
		if a.PTSUs != c.PTSUs {
			return a.PTSUs < c.PTSUs
		}
		return a.FirstTick < c.FirstTick
	})
	pts = Axis{Name: "pts", Frames: pres}
	return dts, pts
}

// Durations returns per-frame presentation gaps in microseconds in PTS order.
// Variable gaps are the VFR signal; the first frame reports its own DTS span.
func Durations(pts Axis) []int64 {
	out := make([]int64, len(pts.Frames))
	for i, f := range pts.Frames {
		switch {
		case i == 0:
			out[i] = f.PTSUs
		default:
			out[i] = f.PTSUs - pts.Frames[i-1].PTSUs
		}
	}
	return out
}

func sortFrames(frames []*Frame, less func(a, b *Frame) bool) {
	for i := 1; i < len(frames); i++ {
		for j := i; j > 0 && less(frames[j], frames[j-1]); j-- {
			frames[j], frames[j-1] = frames[j-1], frames[j]
		}
	}
}
