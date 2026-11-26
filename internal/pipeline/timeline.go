// Package pipeline wires ingest, transport, abr, segment and index modules
// into the deterministic avpipe processing flow, including crash/resume
// semantics and the demo report.
package pipeline

import (
	"avpipe/internal/media"
	"avpipe/internal/transport"
)

// Timeline is the continuous, gap-filled media timeline after recovery.
type Timeline struct {
	Frames       []*media.Frame // exactly FrameCount frames, freeze-filled
	Audio        [][]byte       // one PCM blob per expected audio packet, silence-filled
	GapFrames    map[uint32]bool
	GapAudio     map[uint32]bool
	Recovered    map[uint64]bool // pktKey of FEC-reconstructed packets
	CorruptDrops int
}

// RebuildTimeline unwraps surviving packets, fills unrecovered slots and
// guarantees frame/audio indices stay perfectly continuous across gaps.
func RebuildTimeline(packets []*media.Packet, frameCount, audioCount uint32) *Timeline {
	tl := &Timeline{
		GapFrames: map[uint32]bool{},
		GapAudio:  map[uint32]bool{},
		Recovered: map[uint64]bool{},
	}
	videoBySeq := map[uint32]*media.Packet{}
	audioBySeq := map[uint32]*media.Packet{}
	for _, p := range packets {
		if p.Recovered {
			tl.Recovered[pktKey(p.Kind, p.Seq)] = true
		}
		switch p.Kind {
		case media.KindVideo:
			videoBySeq[p.Seq] = p
		case media.KindAudio:
			audioBySeq[p.Seq] = p
		}
	}

	tl.Frames = make([]*media.Frame, frameCount)
	var lastGood *media.Frame
	for seq := uint32(0); seq < frameCount; seq++ {
		if p, ok := videoBySeq[seq]; ok {
			if f, ok := decodeFrame(p, seq); ok {
				tl.Frames[seq] = f
				lastGood = f
				continue
			}
		}
		// Unrecoverable video slot: freeze the previous frame, else gray.
		tl.GapFrames[seq] = true
		tl.Frames[seq] = freezeOrGray(lastGood, seq)
	}

	tl.Audio = make([][]byte, audioCount)
	silence := make([]byte, media.AudioSamplesPerPacket*2)
	prev := make([]byte, media.AudioSamplesPerPacket*2)
	havePrev := false
	for seq := uint32(0); seq < audioCount; seq++ {
		if p, ok := audioBySeq[seq]; ok {
			if _, _, _, content, ok := transport.UnwrapPayload(p.Payload); ok {
				tl.Audio[seq] = fitPCM(content)
				copy(prev, tl.Audio[seq])
				havePrev = true
				continue
			}
		}
		tl.GapAudio[seq] = true
		// Reuse the previous audio packet (silent comfort fill) or zeros.
		fill := make([]byte, media.AudioSamplesPerPacket*2)
		if havePrev {
			copy(fill, prev)
		} else {
			copy(fill, silence)
		}
		tl.Audio[seq] = fill
	}
	return tl
}

func decodeFrame(p *media.Packet, seq uint32) (*media.Frame, bool) {
	kind, _, pts, content, ok := transport.UnwrapPayload(p.Payload)
	if !ok || kind != media.KindVideo {
		return nil, false
	}
	w, h := sourceDims()
	planeY := w * h
	planeC := (w / 2) * (h / 2)
	if len(content) < planeY+2*planeC {
		return nil, false
	}
	f := &media.Frame{
		Seq: seq, PTS: pts, Width: w, Height: h,
		Y:  append([]byte(nil), content[:planeY]...),
		Cb: append([]byte(nil), content[planeY:planeY+planeC]...),
		Cr: append([]byte(nil), content[planeY+planeC:planeY+2*planeC]...),
	}
	return f, true
}

func freezeOrGray(last *media.Frame, seq uint32) *media.Frame {
	w, h := sourceDims()
	if last != nil {
		f := &media.Frame{Seq: seq, PTS: media.VideoPTS(seq), Width: w, Height: h, Gapped: true}
		f.Y = append([]byte(nil), last.Y...)
		f.Cb = append([]byte(nil), last.Cb...)
		f.Cr = append([]byte(nil), last.Cr...)
		return f
	}
	f := &media.Frame{Seq: seq, PTS: media.VideoPTS(seq), Width: w, Height: h, Gapped: true,
		Y: make([]byte, w*h), Cb: make([]byte, (w/2)*(h/2)), Cr: make([]byte, (w/2)*(h/2))}
	for i := range f.Cb {
		f.Cb[i] = 128
		f.Cr[i] = 128
	}
	return f
}

func fitPCM(in []byte) []byte {
	out := make([]byte, media.AudioSamplesPerPacket*2)
	copy(out, in) // missing tail bytes stay zero
	return out
}

func pktKey(kind media.PacketKind, seq uint32) uint64 {
	return uint64(kind)<<32 | uint64(seq)
}
