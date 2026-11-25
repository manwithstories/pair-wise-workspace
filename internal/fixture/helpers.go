package fixture

import (
	"streamcut/internal/media"
	"streamcut/internal/ts"
)

// emitPSI sends PAT plus the scenario-appropriate PMT on each IDR boundary.
// PSI is thus repeated once per GOP/second. The psi and damaged cases commit
// valid PMT v1 at GOP 3 and present a corrupt CRC v2 at GOP 6 which the
// parser must reject while retaining v1.
func (g *generator) emitPSI(frame, gop int, pat, pmtV0, pmtV1, pmtBad []byte) {
	// PAT first: the parser only recognizes the PMT PID after a valid PAT,
	// so emitting the PMT first would misroute it into PES reassembly.
	g.emit(g.mux.PacketizePSI(ts.PIDPAT, pat))
	switch {
	case (g.cfg.Case == CasePSI || g.cfg.Case == CaseDamaged) && gop == 6:
		g.emit(g.mux.PacketizePSI(PMTPID, pmtBad))
	case (g.cfg.Case == CasePSI || g.cfg.Case == CaseDamaged) && gop >= 3:
		g.emit(g.mux.PacketizePSI(PMTPID, pmtV1))
	default:
		g.emit(g.mux.PacketizePSI(PMTPID, pmtV0))
	}
}

// emitAudio flushes all 20 ms audio frames due up to the current video tick.
func (g *generator) emitAudio(a *AudioSynth, videoTicks, frameTicks, audioTick int64) {
	due := int((videoTicks + frameTicks) / audioTick)
	for a.frame < due {
		fr := a.Next()
		ticks := int64(fr.Index) * audioTick
		pts := g.rawStamp(ticks)
		pes := ts.BuildPES(0xC0, pts, 0, false, fr.PCM)
		g.emit(g.mux.PacketizePES(g.cfg.AudioPID, pes, nil, false))
	}
}

// planSliceSize calibrates the per-frame NAL filler empirically: PES
// packetization rounds each unit up to a 188-byte boundary and emulation
// prevention expands payloads, so a closed-form estimate is inaccurate.
//
// Two one-second probes give two points of the linear per-second model
//
//	bytesPerSecond(s) = A*s + B
//
// where s is the per-frame filler size. The full-duration target then solves
// for s directly; null stuffing closes the sub-packet residual.
func (g *generator) planSliceSize(targetBytes int64, totalFrames, gops int) int {
	// Probe delta must exceed TS packetization granularity (188 bytes/unit).
	const (
		probe    = 200
		probeGap = 256
	)
	b1 := g.measureOnce(probe)
	b2 := g.measureOnce(probe + probeGap)
	a := (b2 - b1) / probeGap // marginal bytes per second per filler byte
	if a < 1 {
		a = 1
	}
	b := b1 - a*probe
	wantPerSecond := targetBytes/g.cfg.DurationS - 19 // one stuffing pkt/s
	s := (wantPerSecond - b) / a
	if s < 8 {
		s = 8
	}
	return int(s)
}

// measureOnce runs a 1-second generation with a fixed slice size and reports
// the resulting natural byte count (no null stuffing). It owns a private
// Config copy: mutating the shared g.cfg would poison later probes and the
// main run.
func (g *generator) measureOnce(size int) int64 {
	cfg := g.cfg
	cfg.DurationS = 1
	cfg.ResumeSplit = 0
	sub := &generator{cfg: cfg, mux: NewMuxer()}
	sub.runWith(size)
	return int64(sub.bytes)
}

// runWith executes the generation loop with an externally fixed slice size.
func (g *generator) runWith(sliceBytes int) {
	cfg := g.cfg
	durationTicks := cfg.DurationS * int64(90_000)
	frameTicks := int64(90_000) / int64(cfg.FPS)
	totalFrames := int(cfg.DurationS) * cfg.FPS
	audio := NewAudioSynth(DefaultAudio(), durationTicks)
	audioTick := DefaultAudio().TicksPerFrame()
	pat := BuildPATSection(1, 0, PMTPID)
	pmt := BuildPMTSection(0, cfg.VideoPID, cfg.VideoPID, cfg.AudioPID, cfg.AudioType)
	for f := 0; f < totalFrames; f++ {
		ticks := int64(f) * frameTicks
		isIDR := f%cfg.GOPFrames == 0
		if isIDR {
			g.emit(g.mux.PacketizePSI(ts.PIDPAT, pat))
			g.emit(g.mux.PacketizePSI(PMTPID, pmt))
		}
		var nal [][]byte
		if isIDR {
			nal = append(nal, SPS(), PPS())
		}
		nal = append(nal, BuildSlice(isIDR, uint(f%16), byte(f), sliceBytes))
		pcr27 := g.rawStamp(ticks) * 300
		pes := ts.BuildPES(0xE0, g.rawStamp(ticks), g.rawStamp(ticks), true, concatNAL(nal))
		g.emit(g.mux.PacketizePES(cfg.VideoPID, pes, &pcr27, isIDR))
		due := int((ticks + frameTicks) / audioTick)
		for audio.frame < due {
			fr := audio.Next()
			ap := ts.BuildPES(0xC0, g.rawStamp(int64(fr.Index)*audioTick), 0, false, fr.PCM)
			g.emit(g.mux.PacketizePES(cfg.AudioPID, ap, nil, false))
		}
	}
}

// stuffTo pads with null-PID (0x1FFF) packets until the byte budget is met.
// Null packets never affect CC tracking, fingerprints or the manifest, so
// they steer actual_bps without perturbing any measured media property.
func (g *generator) stuffTo(targetBytes int64) {
	for int64(g.bytes) < targetBytes {
		pkt := make([]byte, ts.PacketSize)
		pkt[0] = 0x47
		pkt[1] = 0x1F
		pkt[2] = 0xFF
		for i := 4; i < ts.PacketSize; i++ {
			pkt[i] = 0xFF
		}
		g.emit([][]byte{pkt})
	}
}

// concatNAL joins Annex B NAL units; they already include start codes.
func concatNAL(nal [][]byte) []byte {
	n := 0
	for _, x := range nal {
		n += len(x)
	}
	out := make([]byte, 0, n)
	for _, x := range nal {
		out = append(out, x...)
	}
	return out
}

// silenceIntervals converts frame-level ground truth into merged 90 kHz
// intervals, adjacent silent frames collapsed into one [start,end) range.
func silenceIntervals(t AudioTruth, p AudioParams) []Interval {
	var out []Interval
	tick := p.TicksPerFrame()
	start := -1
	for i, silent := range t.SilentFrame {
		if silent && start < 0 {
			start = i
		}
		if !silent && start >= 0 {
			out = append(out, Interval{
				Start: media.Time90(int64(start) * tick),
				End:   media.Time90(int64(i) * tick),
			})
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, Interval{
			Start: media.Time90(int64(start) * tick),
			End:   media.Time90(int64(len(t.SilentFrame)) * tick),
		})
	}
	return out
}
