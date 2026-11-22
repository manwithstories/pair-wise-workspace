// Package fixture deterministically generates all sample data for the demo
// and tests: an RTP frame stream with configurable faults, plus valid,
// duplicate-bearing, cross-day and corrupted subtitle files. No external
// files are referenced.
package fixture

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"time"

	"github.com/pion/rtp"

	"mediasync/internal/clock"
)

// Material parameters: 10 minutes at the given frame rate, keyframes every
// 2s, segments targeting 6s.
const (
	MaterialMinutes = 10
	KeyframeSeconds = 2
	SegmentSeconds  = 6
	baseRTP         = 0x12345678
)

// Fault names accepted by --faults.
const (
	FaultDuplicate = "duplicate"
	FaultReorder   = "reorder"
	FaultBurstLoss = "burst-loss"
)

// Sample is one deterministic generated data set.
type Sample struct {
	Packets      []*rtp.Packet // transmit order, faults applied
	TotalFrames  int64
	KeyframeInt  int64
	SegmentFrames int64
	Keyframes    []int64
	LostFrames   map[int64]bool
	ValidSRT     string // cues incl. one duplicate pair, crosses midnight
	CorruptSRT   string // invalid time, overlap, truncated block
	ValidVTT     string
	tb           clock.Timebase
}

// Generate builds the sample for a seed. Faults is a set of Fault* names.
func Generate(seed int64, tb clock.Timebase, faults map[string]bool) *Sample {
	rng := rand.New(rand.NewSource(seed))
	s := &Sample{
		TotalFrames:   tb.DurationToFrames(MaterialMinutes * time.Minute),
		KeyframeInt:   tb.DurationToFrames(KeyframeSeconds * time.Second),
		SegmentFrames: tb.DurationToFrames(SegmentSeconds * time.Second),
		LostFrames:    map[int64]bool{},
		tb:            tb,
	}
	for f := int64(0); f < s.TotalFrames; f += s.KeyframeInt {
		s.Keyframes = append(s.Keyframes, f)
	}

	// Burst loss: 5 bursts of 3 packets, each starting exactly on a
	// segment-boundary keyframe so 5 of 100 segments lose random access.
	if faults[FaultBurstLoss] {
		perm := rng.Perm(97) // boundary indices 1..97 of 100 segments
		for _, k := range perm[:5] {
			start := int64(k+1) * s.SegmentFrames
			for f := start; f < start+3; f++ {
				s.LostFrames[f] = true
			}
		}
	}

	// Build the stream in transmit order.
	var stream []*rtp.Packet
	for f := int64(0); f < s.TotalFrames; f++ {
		if s.LostFrames[f] {
			continue
		}
		stream = append(stream, s.packet(f))
	}
	if faults[FaultReorder] {
		for i := 0; i+1 < len(stream); i++ {
			if rng.Float64() < 0.15 {
				stream[i], stream[i+1] = stream[i+1], stream[i]
				i++
			}
		}
	}
	if faults[FaultDuplicate] {
		var dup []*rtp.Packet
		for _, p := range stream {
			dup = append(dup, p)
			if rng.Float64() < 0.01 {
				dup = append(dup, p) // retransmitted copy
			}
		}
		stream = dup
	}
	s.Packets = stream

	s.ValidSRT = genSRT(tb, false)
	s.CorruptSRT = genSRT(tb, true)
	s.ValidVTT = genVTT()
	return s
}

func (s *Sample) packet(frame int64) *rtp.Packet {
	payload := make([]byte, 16)
	binary.LittleEndian.PutUint64(payload, uint64(frame))
	keyframe := frame%s.KeyframeInt == 0
	if keyframe {
		payload[8] = 1
	}
	return &rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: uint16(frame),
			Timestamp:      s.tb.FrameToRTP(frame, 0, baseRTP),
			Marker:         keyframe,
		},
		Payload: payload,
	}
}

// srtTime formats ms-since-midnight as HH:MM:SS,mmm with 24h wrap.
func srtTime(ms int64) string {
	ms %= 24 * 3600 * 1000
	return fmt.Sprintf("%02d:%02d:%02d,%03d",
		ms/3600000, (ms/60000)%60, (ms/1000)%60, ms%1000)
}

// genSRT builds cues every 30s starting 23:54:05 (crosses midnight), with
// one duplicate pair; corrupt adds an invalid time, an overlap and a
// truncated trailing block.
func genSRT(tb clock.Timebase, corrupt bool) string {
	base := int64(23*3600+54*60+5) * 1000 // 23:54:05.000
	out := ""
	n := 0
	add := func(startMs, endMs int64, text string) {
		n++
		out += fmt.Sprintf("%d\n%s --> %s\n%s\n\n", n, srtTime(startMs), srtTime(endMs), text)
	}
	for i := 0; i < 20; i++ {
		start := base + int64(i)*30000
		add(start, start+4000, fmt.Sprintf("cue %d", i))
		if i == 4 {
			add(start, start+4000, "cue 4 duplicate") // ambiguous pair
		}
	}
	if corrupt {
		out += "91\n00:00:61,000 --> 00:00:02,000\ninvalid seconds\n\n"
		// Overlaps the immediately preceding accepted cue (00:03:35-00:03:39).
		out += "92\n00:03:36,000 --> 00:03:38,000\noverlaps previous cue\n\n"
		out += "93\n" // truncated block at EOF
	}
	return out
}

func genVTT() string {
	out := "WEBVTT\n\n"
	for i := 0; i < 5; i++ {
		start := int64(i)*60000 + 1000
		out += fmt.Sprintf("cue-%d\n00:%02d:%02d.%03d --> 00:%02d:%02d.%03d\nvtt cue %d\n\n",
			i, start/60000, (start/1000)%60, start%1000,
			(start+5000)/60000, ((start+5000)/1000)%60, (start+5000)%1000, i)
	}
	return out
}
