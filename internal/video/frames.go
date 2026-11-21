// Package video reconstructs frame boundaries from recovered RTP packets,
// keeping separate decode (DTS) and presentation (PTS) timelines so variable
// frame rates and presentation-order reordering stay explicit.
package video

import (
	"encoding/binary"
	"fmt"
	"sort"
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

// maxPendingFrames bounds the frame assembly window so memory stays bounded.
// It matches the RTP reorder window expressed in frames (10 packets/frame).
const maxPendingFrames = 64

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

type pending struct {
	frame  *Frame
	dts    uint32
	cto    uint32
	marked bool // marker observed; late same-frame packets are still absorbed
}

// Builder accumulates packets keyed by frame ID, tolerating intra-frame
// reordering, and emits frames in frame-ID order. Memory is bounded by
// maxPendingFrames; a frame that overflows the window is flushed early.
type Builder struct {
	pending map[uint32]map[uint32]*pending // ssrc -> frame ID -> pending
	order   map[uint32][]uint32            // ssrc -> frame IDs in arrival order
}

// NewBuilder creates an empty frame builder.
func NewBuilder() *Builder {
	return &Builder{pending: map[uint32]map[uint32]*pending{}, order: map[uint32][]uint32{}}
}

// Push folds one packet into its frame. A frame is finalized when the next
// frame ID starts (the previous frame then cannot gain more packets) or when
// the assembly window overflows, so a marker arriving before its same-frame
// peers never closes the frame prematurely. It may return one frame.
func (b *Builder) Push(in PacketInput) (*Frame, error) {
	if len(in.Payload) < headerLen {
		return nil, fmt.Errorf("video: %d-byte payload shorter than header tick=%d", len(in.Payload), in.Tick)
	}
	id := uint32(binary.BigEndian.Uint16(in.Payload[0:2]))
	flags := in.Payload[2]
	cto := binary.BigEndian.Uint32(in.Payload[3:7])
	frames, ok := b.pending[in.SSRC]
	if !ok {
		frames = map[uint32]*pending{}
		b.pending[in.SSRC] = frames
	}
	// A new frame ID finalizes any marked frame for this SSRC: its packet
	// run is provably complete. Unmarked predecessors are kept pending.
	var completed *Frame
	if _, exists := frames[id]; !exists {
		if prevID, ok := b.markedHead(in.SSRC); ok && prevID != id {
			completed = b.close(in.SSRC, prevID)
		}
	}
	p, ok := frames[id]
	if !ok {
		p = &pending{frame: &Frame{ID: id, SSRC: in.SSRC, FirstTick: in.Tick}, dts: in.Timestamp, cto: cto}
		frames[id] = p
		b.order[in.SSRC] = append(b.order[in.SSRC], id)
	}
	p.frame.Packets = append(p.frame.Packets, in.Seq)
	if flags&flagBad != 0 {
		p.frame.Bad = true
	}
	if flags&flagKeyframe != 0 {
		p.frame.Keyframe = true
	}
	if in.Marker {
		p.marked = true
	}
	// Window overflow: evict the oldest frame so memory stays bounded.
	if len(frames) > maxPendingFrames {
		oldest := b.order[in.SSRC][0]
		if f := b.close(in.SSRC, oldest); completed == nil {
			completed = f
		}
	}
	return completed, nil
}

// markedHead returns the oldest frame ID whose marker has been observed.
func (b *Builder) markedHead(ssrc uint32) (uint32, bool) {
	for _, id := range b.order[ssrc] {
		if p, ok := b.pending[ssrc][id]; ok && p.marked {
			return id, true
		}
	}
	return 0, false
}

// close finalizes one frame and removes it from the pending window.
func (b *Builder) close(ssrc uint32, id uint32) *Frame {
	frames := b.pending[ssrc]
	p, ok := frames[id]
	if !ok {
		return nil
	}
	p.frame.DTSUs = int64(float64(p.dts) / ClockRate * 1e6)
	p.frame.PTSUs = int64(float64(uint64(p.dts)+uint64(p.cto)) / ClockRate * 1e6)
	delete(frames, id)
	if ids := b.order[ssrc]; len(ids) > 0 && ids[0] == id {
		b.order[ssrc] = ids[1:]
	} else {
		for i, v := range b.order[ssrc] {
			if v == id {
				b.order[ssrc] = append(b.order[ssrc][:i], b.order[ssrc][i+1:]...)
				break
			}
		}
	}
	return p.frame
}

// Flush emits every frame still pending at end of stream in frame-ID order.
func (b *Builder) Flush() []*Frame {
	var out []*Frame
	for ssrc, frames := range b.pending {
		ids := make([]uint32, 0, len(frames))
		for id := range frames {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids {
			if f := b.close(ssrc, id); f != nil {
				out = append(out, f)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SSRC != out[j].SSRC {
			return out[i].SSRC < out[j].SSRC
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Axis is one time-ordered view over recovered frames.
type Axis struct {
	Name   string
	Frames []*Frame
}

// Axes builds the decode and presentation axes. The DTS axis follows frame
// (decode) order; the PTS axis is sorted by presentation time, which lets B
// frames and VFR cadence diverge without mutating frame identity.
func Axes(frames []*Frame) (dts Axis, pts Axis) {
	dts = Axis{Name: "dts", Frames: append([]*Frame(nil), frames...)}
	pres := append([]*Frame(nil), frames...)
	sort.Slice(pres, func(i, j int) bool {
		if pres[i].PTSUs != pres[j].PTSUs {
			return pres[i].PTSUs < pres[j].PTSUs
		}
		return pres[i].FirstTick < pres[j].FirstTick
	})
	pts = Axis{Name: "pts", Frames: pres}
	return dts, pts
}

// Durations returns per-frame presentation gaps in microseconds in PTS order.
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
