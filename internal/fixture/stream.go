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
	cfg     Config
	mux     *Muxer
	packets [][]byte
	tail    [][]byte
	bytes   int
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
		if g.cfg.ResumeSplit > 0 && len(g.packets) >= g.cfg.ResumeSplit {
			g.tail = append(g.tail, pkt)
			continue
		}
		g.packets = append(g.packets, pkt)
	}
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

	res := &Result{
		Packets: g.packets, Tail: g.tail, Bytes: g.bytes,
		Truth: audio.Truth(), VideoGOPs: gops,
		Segments: int(cfg.DurationS) / 2,
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

	durationTicks := cfg.DurationS * int64(media.TicksPerSecond)
	frameTicks := int64(media.TicksPerSecond) / int64(cfg.FPS)
	totalFrames := int(cfg.DurationS) * cfg.FPS
	gops := totalFrames / cfg.GOPFrames

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
		// Second half of the program replays the first half byte-for-byte,
		// producing exact fingerprint duplicates.
		tmplGOP := gop
		if tmplGOP >= (gops+1)/2 {
			tmplGOP = gop - (gops+1)/2
		}
		seed := byte(17 + tmplGOP*7 + f%cfg.GOPFrames)

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
		nal = append(nal, BuildSlice(isIDR, uint(f%16), seed, sliceBytes))
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
