package fixture

import (
	"streamcut/internal/media"
	"streamcut/internal/ts"
)

// Case selects the synthesized fault scenario.
type Case string

const (
	CaseClean   Case = "clean"
	CaseWrap    Case = "wrap"    // PCR/PTS wrap past 2^33
	CaseCC      Case = "cc"      // continuity counter jump
	CasePSI     Case = "psi"     // valid version bump then bad-CRC version
	CaseTrunc   Case = "trunc"   // input ends mid-packet
	CaseDamaged Case = "damaged" // wrap + CC + PSI CRC rejection + truncation
	CaseResume  Case = "resume"  // clean-equivalent stream; split for restart
)

// Config drives one deterministic generation run.
type Config struct {
	Seed       int64
	TargetBPS  int64
	DurationS  int64
	FPS        int
	GOPFrames  int
	WidthMB    int
	HeightMB   int
	VideoPID   uint16
	AudioPID   uint16
	AudioType  ts.StreamType // 0x06 PCM, 0x0F AAC (unsupported)
	Case       Case
	// ResumeSplit cuts the stream into head/tail at this packet count for
	// the interrupted half of a resume demo. 0 means no split.
	ResumeSplit int
}

// DefaultConfig returns the 10 s / 30 fps / 1 s GOP configuration used by
// the demo commands.
func DefaultConfig(seed int64, targetBPS int64, c Case) Config {
	return Config{
		Seed: seed, TargetBPS: targetBPS, DurationS: 10, FPS: 30,
		GOPFrames: 30, WidthMB: 20, HeightMB: 12,
		VideoPID: VideoPID, AudioPID: AudioPID,
		AudioType: ts.STPCMPrivate, Case: c,
	}
}

// Result carries generated packets plus the ground truth a demo needs.
type Result struct {
	Packets   [][]byte
	Tail      [][]byte // packets after ResumeSplit when configured
	Bytes     int
	Truth     AudioTruth
	VideoGOPs int
	Segments  int // expected IDR-aligned segment count at 2 s target
	Silence   []Interval
	// TruncatedFlag is true when the final packet was deliberately cut short.
	TruncatedFlag bool
	// SplitRawPTS is the raw modulo-33 video PTS of the tail's first IDR, and
	// SplitExtendedPTS its position on the zero-origin extended timeline. Both
	// are zero unless a ResumeSplit was configured and armed.
	SplitRawPTS      uint64
	SplitExtendedPTS media.Time90
}

// Interval is a time range in extended 90 kHz ticks.
type Interval struct {
	Start media.Time90
	End   media.Time90
}

const mod33 = int64(1) << 33

// Generate builds the full packet stream deterministically.
func Generate(cfg Config) (*Result, error) {
	g := &generator{cfg: cfg, mux: NewMuxer()}
	return g.run()
}

type generator struct {
	cfg             Config
	mux             *Muxer
	packets         [][]byte
	tail            [][]byte
	bytes           int
	splitArmed      bool
	splitRawPTS     uint64
	splitExtendedPTS media.Time90
}

// wraps reports whether the scenario crosses the 33-bit modulus.
func (g *generator) wraps() bool {
	return g.cfg.Case == CaseWrap || g.cfg.Case == CaseDamaged
}

// baseTicks places the clock two seconds before the wrap point for wrap
// scenarios so the modulus is crossed mid-program; otherwise starts at zero.
func (g *generator) baseTicks() int64 {
	if g.wraps() {
		return mod33 - 2*int64(media.TicksPerSecond)
	}
	return 0
}

// rawStamp returns the on-wire modulo-33 PTS/PCR-base for a given frame.
func (g *generator) rawStamp(ticks int64) uint64 {
	return uint64((g.baseTicks() + ticks) & (mod33 - 1))
}

func (g *generator) emit(p [][]byte) {
	for _, pkt := range p {
		g.bytes += len(pkt)
		if g.cfg.ResumeSplit > 0 {
			// Defer the split until the next IDR that lands exactly on a
			// 2-second segment boundary, so head and tail never split one
			// segment: every cut on either side matches the uninterrupted run.
			// Once armed, every later packet belongs to the tail.
			if !g.splitArmed {
				if len(g.packets) >= g.cfg.ResumeSplit && g.isSegmentBoundaryIDR(pkt) {
					g.splitArmed = true
					g.recordSplit(pkt)
				}
			}
			if g.splitArmed {
				g.tail = append(g.tail, pkt)
				continue
			}
		}
		g.packets = append(g.packets, pkt)
	}
}

// recordSplit captures the tail boundary from the IDR packet's PCR: the raw
// modulo-33 video PTS and its zero-origin extended position in ticks.
func (g *generator) recordSplit(pkt []byte) {
	parsed, err := ts.Parse(pkt)
	if err != nil || !parsed.HasPCR {
		return
	}
	rawBase := parsed.PCR / 300
	g.splitRawPTS = rawBase
	// Recover the zero-origin tick from the on-wire stamp using the scenario
	// base offset; the inverse of rawStamp().
	ext := int64(rawBase) - g.baseTicks()
	if ext < 0 {
		ext += mod33 // crossed the 33-bit modulus
	}
	g.splitExtendedPTS = media.Time90(ext)
}

// isVideoIDR reports whether a packet starts a video IDR access unit: it is on
// the video PID, carries PUSI and the random-access adaptation indicator.
func (g *generator) isVideoIDR(pkt []byte) bool {
	const pidMask = 0x1FFF
	pid := (uint16(pkt[1]&0x1F) << 8) | uint16(pkt[2])
	if pid != g.cfg.VideoPID || pkt[1]&0x40 == 0 || pkt[3]&0x20 == 0 {
		return false
	}
	alen := int(pkt[4])
	if alen < 1 {
		return false
	}
	return pkt[5]&0x40 != 0 // random_access_indicator in adaptation flags
}

// segmentTicks is the 2-second target segment length in 90 kHz ticks.
const segmentTicks = int64(180_000)

// isSegmentBoundaryIDR reports a video IDR whose PCR sits on a 2-second
// segment boundary, so a resume split there never bisects a segment.
func (g *generator) isSegmentBoundaryIDR(pkt []byte) bool {
	if !g.isVideoIDR(pkt) {
		return false
	}
	parsed, err := ts.Parse(pkt)
	if err != nil || !parsed.HasPCR {
		return false
	}
	rawBase := int64(parsed.PCR / 300)
	ext := rawBase - g.baseTicks()
	if ext < 0 {
		ext += mod33
	}
	return ext%segmentTicks == 0
}

func (g *generator) run() (*Result, error) {
	cfg := g.cfg
	totalFrames := int(cfg.DurationS) * cfg.FPS
	gops := totalFrames / cfg.GOPFrames
	targetBytes := cfg.TargetBPS * cfg.DurationS / 8

	// Calibrate the filler size against a real build: probe-based linear
	// estimates drift because of packetization rounding, so measure the
	// actual natural size and correct once. Two iterations converge within
	// one TS packet.
	sliceBytes := g.planSliceSize(targetBytes, totalFrames, gops)
	for attempt := 0; attempt < 2; attempt++ {
		nat, _ := g.buildOnce(sliceBytes, false)
		if nat <= targetBytes {
			break // stuffing will close the gap
		}
		sliceBytes = int(float64(sliceBytes) *
			float64(targetBytes-188) / float64(nat))
		if sliceBytes < 8 {
			sliceBytes = 8
		}
	}

	// Final build with null stuffing to land exactly on the byte budget.
	_, audio := g.buildOnce(sliceBytes, true)
	g.stuffTo(targetBytes)

	// trunc/damaged scenarios end mid-packet: replace the final complete
	// packet with a shortened one so the parser reports ErrTruncated and the
	// operator can distinguish a recoverable cut from structural corruption.
	truncated := false
	if cfg.Case == CaseTrunc || cfg.Case == CaseDamaged {
		if n := len(g.packets); n > 0 {
			last := g.packets[n-1]
			g.packets[n-1] = last[:len(last)-40]
			g.bytes -= 40
			truncated = true
		}
	}

	res := &Result{
		Packets: g.packets, Tail: g.tail, Bytes: g.bytes,
		Truth: audio.Truth(), VideoGOPs: gops,
		Segments:        int(cfg.DurationS) / 2,
		TruncatedFlag:   truncated,
		SplitRawPTS:     g.splitRawPTS,
		SplitExtendedPTS: g.splitExtendedPTS,
	}
	res.Silence = silenceIntervals(audio.Truth(), DefaultAudio())
	return res, nil
}

// buildOnce executes one complete generation with a fixed per-frame filler
// size. When stuff is true null packets pad to the byte budget; otherwise the
// natural byte total is returned for calibration. The receiver is reset so
// repeated builds do not accumulate.
func (g *generator) buildOnce(sliceBytes int, stuff bool) (int64, *AudioSynth) {
	cfg := g.cfg
	g.mux = NewMuxer()
	g.packets = nil
	g.tail = nil
	g.bytes = 0
	g.splitArmed = false
	g.splitRawPTS = 0
	g.splitExtendedPTS = 0

	durationTicks := cfg.DurationS * int64(media.TicksPerSecond)
	frameTicks := int64(media.TicksPerSecond) / int64(cfg.FPS)
	totalFrames := int(cfg.DurationS) * cfg.FPS

	pat := BuildPATSection(1, 0, PMTPID)
	pmtV0 := BuildPMTSection(0, cfg.VideoPID, cfg.VideoPID, cfg.AudioPID, cfg.AudioType)
	pmtV1 := BuildPMTSection(1, cfg.VideoPID, cfg.VideoPID, cfg.AudioPID, cfg.AudioType)
	pmtBad := CorruptSection(BuildPMTSection(2, cfg.VideoPID,
		cfg.VideoPID, cfg.AudioPID, cfg.AudioType))

	audio := NewAudioSynth(DefaultAudio(), durationTicks)
	audioTick := DefaultAudio().TicksPerFrame()

	for f := 0; f < totalFrames; f++ {
		ticks := int64(f) * frameTicks
		isIDR := f%cfg.GOPFrames == 0
		gop := f / cfg.GOPFrames
		// Content repeats every two GOPs (the 2 s segment period), so a
		// segment's media bytes recur later in the program and fingerprint
		// dedup produces exact hits. The seed depends only on the frame's
		// position inside the repeating window, never on the wall clock.
		const segmentFrames = 60 // 2 GOPs at 30 fps
		windowPos := f % segmentFrames
		seed := byte(17 + windowPos)

		if isIDR {
			g.emitPSI(f, gop, pat, pmtV0, pmtV1, pmtBad)
		}
		// cc/damaged: force one CC jump on the video PID at frame 150.
		if (cfg.Case == CaseCC || cfg.Case == CaseDamaged) && f == 150 {
			g.mux.ForceCCJump(cfg.VideoPID)
		}

		pcrBase := g.rawStamp(ticks)
		pcr27 := pcrBase*300 + uint64(f%300)
		var nal [][]byte
		if isIDR {
			nal = append(nal, SPS(), PPS())
		}
		nal = append(nal, BuildSlice(isIDR, uint(windowPos%16), seed, sliceBytes))
		pts := g.rawStamp(ticks)
		pes := ts.BuildPES(0xE0, pts, pts, true, concatNAL(nal))
		g.emit(g.mux.PacketizePES(cfg.VideoPID, pes, &pcr27, isIDR))

		// Emit every 20 ms audio frame whose interval ends by this video PTS.
		g.emitAudio(audio, ticks, frameTicks, audioTick)
	}
	if stuff {
		g.stuffTo(cfg.TargetBPS * cfg.DurationS / 8)
	}
	return int64(g.bytes), audio
}
