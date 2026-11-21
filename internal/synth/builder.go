// Package synth generates the deterministic RTP/RTCP capture used by the
// pipeline, demo and tests. No external media files are involved: every
// packet, sender report and ground-truth label derives from a seed.
package synth

import (
	"math/rand"
)

// Payload types assigned by the synthetic capture.
const (
	VideoPT = 96
	AudioPT = 97
	SRPT    = 200
)

// SSRCs and clock rates for the two media streams.
const (
	VideoSSRC uint32 = 0x56494445 // "VIDE"
	AudioSSRC uint32 = 0x41554449 // "AUDI"
	VideoRate        = 90000
	AudioRate        = 48000
	// KeyframeEvery video frames between keyframes.
	KeyframeEvery = 12
	// VPerFrame video packets make up one video frame.
	VPerFrame = 10
)

// Flags carried in the synthetic per-packet payload header.
const (
	FlagKeyframe = 0x01
	FlagBad      = 0x02
)

// HeaderLen is frame-id (2) + flags (1) + composition offset (4).
const HeaderLen = 7

// Record is one generated datagram before network transforms.
type Record struct {
	Kind      uint8 // 0 RTP, 1 RTCP
	SSRC      uint32
	Seq       uint16
	Timestamp uint32
	Marker    bool
	Payload   []byte
	// Ground truth.
	FrameID  uint32
	Keyframe bool
	Bad      bool
	NTPTime  uint64
}

// Config controls capture content and network degradation.
type Config struct {
	Seed           int64
	VideoPackets   int
	AudioPackets   int
	LossPercent    int
	ReorderPercent int
	DuplicatePct   int
	WrapAt         uint64 // 0 => full 2^32 modulus
	DriftPPM       int64
	VFR            bool
	BadFrameEvery  int // 0 disables synthetic corruption
}

// Truth is the synthetic ground truth returned alongside records.
type Truth struct {
	KeyframeIDs []uint32
	BadFrameIDs map[uint32]bool
	Frames      []FrameTruth
}

// FrameTruth labels one generated video frame.
type FrameTruth struct {
	ID       uint32
	Keyframe bool
	Bad      bool
	SeqStart uint16
}

// Builder produces an in-order capture, then applies network transforms.
type Builder struct {
	cfg Config
	rng *rand.Rand
}

// NewBuilder creates a seeded builder.
func NewBuilder(cfg Config) *Builder {
	if cfg.VideoPackets <= 0 {
		cfg.VideoPackets = 600
	}
	if cfg.AudioPackets <= 0 {
		cfg.AudioPackets = 600
	}
	return &Builder{cfg: cfg, rng: rand.New(rand.NewSource(cfg.Seed))}
}

// payload builds the 7-byte synthetic header plus deterministic filler.
func payload(id uint16, flags byte, cto uint32, n int) []byte {
	p := make([]byte, HeaderLen+n)
	p[0], p[1] = byte(id>>8), byte(id)
	p[2] = flags
	p[3], p[4], p[5], p[6] = byte(cto>>24), byte(cto>>16), byte(cto>>8), byte(cto)
	for i := HeaderLen; i < len(p); i++ {
		p[i] = byte((int(id)*7 + i) & 0xFF)
	}
	return p
}

// Build generates video+audio RTP records interleaved with RTCP sender
// reports, applies loss/reorder/duplication, and returns ground truth.
func (b *Builder) Build() ([]Record, Truth) {
	cfg := b.cfg
	truth := Truth{BadFrameIDs: map[uint32]bool{}}
	var recs []Record
	vSeq, aSeq := uint16(0), uint16(0)
	// Unwrapped counters run in exact clock units; drift is folded into each
	// increment and the wrap modulus is only applied at emission.
	var vRaw, aRaw float64
	frameID := uint32(0)
	ntpSec := uint64(1)
	scale := 1 + float64(cfg.DriftPPM)/1e6
	for i := 0; i < cfg.VideoPackets; i++ {
		inFrame := i % VPerFrame
		if inFrame == 0 {
			key := frameID%KeyframeEvery == 0
			bad := cfg.BadFrameEvery > 0 && int(frameID)%cfg.BadFrameEvery == 0 && !key
			flags := byte(0)
			if key {
				flags |= FlagKeyframe
				truth.KeyframeIDs = append(truth.KeyframeIDs, frameID)
			}
			if bad {
				flags |= FlagBad
				truth.BadFrameIDs[frameID] = true
			}
			truth.Frames = append(truth.Frames, FrameTruth{ID: frameID, Keyframe: key, Bad: bad, SeqStart: vSeq})
			if frameID > 0 {
				step := uint32(3000) // 33.3 ms at 90 kHz
				if cfg.VFR && frameID%2 == 1 {
					step = 4000
				}
				vRaw += float64(step) * scale
			}
			frameID++
		}
		fid := uint16(frameID - 1)
		ft := truth.Frames[len(truth.Frames)-1]
		flags := byte(0)
		if ft.Keyframe {
			flags |= FlagKeyframe
		}
		if ft.Bad {
			flags |= FlagBad
		}
		recs = append(recs, Record{
			SSRC: VideoSSRC, Seq: vSeq, Timestamp: b.wrap(uint64(vRaw)),
			Marker: inFrame == VPerFrame-1, Payload: payload(fid, flags, 0, 4),
			FrameID: uint32(fid), Keyframe: ft.Keyframe, Bad: ft.Bad,
		})
		vSeq++
		// One 20 ms audio packet (960 samples) per two video packets.
		if i%2 == 1 && audioCount(recs) < cfg.AudioPackets {
			aRaw += 960 * scale
			recs = append(recs, Record{
				SSRC: AudioSSRC, Seq: aSeq, Timestamp: b.wrap(uint64(aRaw)),
				Marker: true, Payload: payload(0, 0, 0, 8),
			})
			aSeq++
		}
		// RTCP SR pair roughly every 50 video packets.
		if i > 0 && i%50 == 0 {
			ntp := ntpSec << 32
			recs = append(recs, Record{Kind: 1, SSRC: VideoSSRC, NTPTime: ntp, Timestamp: b.wrap(uint64(vRaw))})
			recs = append(recs, Record{Kind: 1, SSRC: AudioSSRC, NTPTime: ntp, Timestamp: b.wrap(uint64(aRaw))})
			ntpSec++
		}
	}
	recs = b.applyNetwork(recs)
	return recs, truth
}

func audioCount(recs []Record) int {
	n := 0
	for _, r := range recs {
		if r.Kind == 0 && r.SSRC == AudioSSRC {
			n++
		}
	}
	return n
}

// wrap folds an unwrapped timestamp at the configured modulus.
func (b *Builder) wrap(v uint64) uint32 {
	if b.cfg.WrapAt == 0 {
		return uint32(v)
	}
	return uint32(v % b.cfg.WrapAt)
}
