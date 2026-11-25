package synth

import (
	"fmt"

	"cinepipe/internal/clock"
	"cinepipe/internal/ts"
)

// SampleKind selects one of the acceptance fixtures.
type SampleKind string

const (
	// SampleNormal is a clean stream: dense keyframes, monotonic PCR, no gaps.
	SampleNormal SampleKind = "normal"
	// SampleWrap layers a PCR/PTS modulus wrap over a PSI version update.
	SampleWrap SampleKind = "pcr-wrap-psi"
	// SampleContinuity drops packets on the video PID so continuity counters
	// jump and the scanner must close a damaged range.
	SampleContinuity SampleKind = "continuity"
	// SampleTruncated cuts the final packet short.
	SampleTruncated SampleKind = "truncated"
	// SampleMetadata changes audio parameters midway so descriptions conflict.
	SampleMetadata SampleKind = "metadata"
)

// Sample is a generated fixture with the facts a test needs to assert on.
type Sample struct {
	Kind         SampleKind
	Data         []byte
	VideoPID     uint16
	AudioPID     uint16
	PMTPID       uint16
	// KeyframeTicks lists intended keyframe timestamps relative to the first
	// frame, in 90 kHz ticks.
	KeyframeTicks []int64
	// FrameTicks lists every intended frame timestamp similarly.
	FrameTicks []int64
	// PCRBase is the clock value used for the first PCR sample.
	PCRBase uint64
	// KeyframeInterval is the nominal GOP length in ticks.
	KeyframeInterval int64
	// IntendedDrops is how many video packets the continuity sample removes.
	IntendedDrops int
	// IntendedTruncation is how many trailing bytes the truncated sample cuts.
	IntendedTruncation int
}

// GenOptions tunes the generator.
type GenOptions struct {
	Seed          int64
	Frames        int
	FPS           int
	GOP           int
	AudioChannels int
	AudioRate     int
	// PCRBase seeds the PCR counter; set near the modulus for a wrap.
	PCRBase uint64
	// PSIBumps is how many mid-stream PSI version advances to make.
	PSIBumps int
	// DropPeriod, when non-zero, drops DropCount consecutive video packets at
	// every DropPeriod-th frame.
	DropCount  int
	DropPeriod int
	// TruncateTail cuts this many bytes from the end.
	TruncateTail int
	// MetadataSwitch changes audio channel/rate at this frame index; -1 disables.
	MetadataSwitch int
}

// Defaults returns the option set for a kind, seeded so every run matches.
func Defaults(kind SampleKind, seed int64) GenOptions {
	o := GenOptions{Seed: seed, FPS: 30, AudioChannels: 2, AudioRate: 48000, MetadataSwitch: -1}
	switch kind {
	case SampleNormal:
		o.Frames, o.GOP = 60, 12
	case SampleWrap:
		o.Frames, o.GOP = 90, 15
		o.PCRBase = clock.PCRModulus - 400000
		o.PSIBumps = 1
	case SampleContinuity:
		o.Frames, o.GOP = 90, 15
		o.DropCount, o.DropPeriod = 2, 15
	case SampleTruncated:
		o.Frames, o.GOP = 60, 12
		o.TruncateTail = 60
	case SampleMetadata:
		o.Frames, o.GOP = 60, 12
		o.AudioChannels, o.AudioRate = 2, 48000
		o.MetadataSwitch = 30
	}
	return o
}

// Build synthesises the sample stream. The frame plan is laid out first so
// exact keyframe positions are known, then packets are emitted with PCR, PTS,
// audio, PSI refreshes, and any injected faults.
func Build(kind SampleKind, opts GenOptions) *Sample {
	if opts.FPS <= 0 {
		opts.FPS = 30
	}
	if opts.GOP <= 0 {
		opts.GOP = 12
	}
	rng := NewRand(opts.Seed)
	m := NewMuxer(Options{
		AudioType:     ts.StreamTypeAAC,
		AudioRate:     opts.AudioRate,
		AudioChannels: opts.AudioChannels,
	})
	s := &Sample{
		Kind:     kind,
		VideoPID: m.VideoPID(),
		AudioPID: m.AudioPID(),
		PMTPID:   m.PMTPID(),
		PCRBase:  opts.PCRBase,
	}
	frameTicks := int64(clock.Base) / int64(opts.FPS)
	s.KeyframeInterval = frameTicks * int64(opts.GOP)

	// Timeline origin: the first video PTS. PCR trails it by one second.
	origin := int64(clock.Base)
	pcrNow := int64(opts.PCRBase)

	writePCR := func() {
		m.WritePCR(m.PCRPID(), pcrNow, false)
	}

	m.WritePSI()
	writePCR()

	patEvery := opts.Frames / 4
	if patEvery < 1 {
		patEvery = 1
	}
	bumps := 0
	for i := 0; i < opts.Frames; i++ {
		pts := origin + int64(i)*frameTicks
		rel := int64(i) * frameTicks
		key := i%opts.GOP == 0
		s.FrameTicks = append(s.FrameTicks, rel)
		if key {
			s.KeyframeTicks = append(s.KeyframeTicks, rel)
		}

		// PCR advances in step with the video clock.
		pcrNow = int64(opts.PCRBase) + rel
		writePCR()

		unit := videoAccessUnit(rng, key, 900+int(rng.Intn(200)))
		m.WritePES(m.VideoPID(), 0xE0, unit, pts, true)

		if opts.DropPeriod > 0 && (i+1)%opts.DropPeriod == 0 {
			dropped := m.DropLastPackets(m.VideoPID(), opts.DropCount)
			s.IntendedDrops += dropped
		}

		channels, rate := opts.AudioChannels, opts.AudioRate
		if opts.MetadataSwitch >= 0 && i >= opts.MetadataSwitch {
			channels, rate = 6, 44100
		}
		frame := adtsFrame(rng, rate, channels)
		m.WritePES(m.AudioPID(), 0xC0, frame, pts, true)

		if i%patEvery == 0 && i != 0 {
			if bumps < opts.PSIBumps && i >= opts.Frames/2 {
				m.BumpPSIVersion()
				bumps++
			}
			m.WritePSI()
		}
	}

	data := m.Bytes()
	if opts.TruncateTail > 0 && len(data) > opts.TruncateTail {
		data = data[:len(data)-opts.TruncateTail]
		s.IntendedTruncation = opts.TruncateTail
	}
	s.Data = data
	return s
}

// videoAccessUnit builds an H.264 Annex-B access unit: an optional SPS/PPS
// pair before an IDR, then either an IDR or a non-IDR slice. Payload sizes come
// from the seeded RNG so frame byte counts vary like real footage. Slice bytes
// are drawn from 0x10..0xEF so filler can never form a false 00 00 01 start
// code, which would desynchronise the whole parse.
func videoAccessUnit(rng *SeededRand, key bool, sliceSize int) []byte {
	nal := func(header byte, n int) []byte {
		b := make([]byte, 0, 5+n)
		b = append(b, 0, 0, 0, 1, header)
		for i := 0; i < n; i++ {
			b = append(b, byte(0x10+rng.Intn(0xE0)))
		}
		return b
	}
	unit := nal(0x09, 3) // AUD
	if key {
		unit = append(unit, nal(0x67, 8)...)         // SPS
		unit = append(unit, nal(0x68, 4)...)         // PPS
		unit = append(unit, nal(0x65, sliceSize)...) // IDR
	} else {
		unit = append(unit, nal(0x41, sliceSize)...) // non-IDR slice
	}
	return unit
}

// adtsFrame builds one AAC ADTS frame with filler payload of deterministic size.
// Filler bytes stay in 0x10..0xEF so no accidental start code forms.
func adtsFrame(rng *SeededRand, rate, channels int) []byte {
	sfi := ts.SampleRateIndex(rate)
	if sfi < 0 {
		sfi = 3 // 48000 default when an unsupported rate is requested
	}
	frameLen := 7 + 8 + rng.Intn(40)
	hdr := ts.BuildADTSHeader(1, uint8(sfi), uint8(channels), frameLen)
	payload := make([]byte, frameLen-7)
	for i := range payload {
		payload[i] = byte(0x10 + rng.Intn(0xE0))
	}
	return append(hdr, payload...)
}

// RTPPacket renders one TS packet as an RTP payload for the capture fixtures.
func RTPPacket(seq uint16, tsPayload []byte) []byte {
	b := make([]byte, 12, 12+len(tsPayload))
	b[0] = 0x80 // version 2
	b[1] = 33   // MP2T payload type
	b[2] = uint8(seq >> 8)
	b[3] = uint8(seq)
	b[4] = uint8(seq >> 8)
	b[5] = uint8(seq)
	b[8], b[9], b[10], b[11] = 0x12, 0x34, 0x56, 0x78
	return append(b, tsPayload...)
}

// Info renders a compact one-line description for the demo.
func Info(kind SampleKind, s *Sample) string {
	return fmt.Sprintf("%s bytes=%d keyframes=%d frames=%d",
		kind, len(s.Data), len(s.KeyframeTicks), len(s.FrameTicks))
}
