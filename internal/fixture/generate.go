package fixture

import (
	"fmt"

	"mediaflow/internal/timeline"
)

// FECGroupSize is the media-packet count protected by one parity packet.
const FECGroupSize = 8

// Hard limits enforced on every generated / consumed clip.
const (
	MaxTracks       = 16
	MaxPackets      = 100_000
	MaxPayloadBytes = 4096
	MaxEvents       = 10_000
)

// defaultTracks builds the four canonical demo tracks.
func defaultTracks() []Track {
	return []Track{
		{ID: 1, Kind: KindVideo, Timebase: NumDen{1, 90000}, DriftPPM: 120, PayloadLen: 256},
		{ID: 2, Kind: KindAudio, Timebase: NumDen{1, 48000}, DriftPPM: 40, PayloadLen: 192},
		{ID: 3, Kind: KindSubtitle, Timebase: NumDen{1, 1000}, PayloadLen: 96},
		{ID: 4, Kind: KindChapter, Timebase: NumDen{1, 1000}, PayloadLen: 64},
	}
}

// Generate builds the undamaged packet/event stream for cfg. Damage is applied
// separately by ApplyDamage so the clean case shares identical source data.
func Generate(cfg Config) (*Clip, error) {
	if len(defaultTracks()) > MaxTracks {
		return nil, timeline.GlobalError("fixture.generate", false,
			fmt.Errorf("%w: tracks", timeline.ErrLimit))
	}
	tracks := defaultTracks()
	clip := &Clip{Seed: cfg.Seed, Tracks: tracks}

	videoTB := mustTB(tracks[0].Timebase)
	audioTB := mustTB(tracks[1].Timebase)
	metaND := tracks[2].Timebase

	// Video: VFR frame durations around the nominal 1/fps interval.
	frameUS := int64(1_000_000 * cfg.VideoFPS.Den / cfg.VideoFPS.Num)
	var curUS int64
	for f := 0; f < cfg.VideoFrames; f++ {
		durUS := frameUS
		if cfg.VFR {
			// +/-20% deterministic jitter, bounded to keep ordering monotonic.
			r := newRand(cfg.Seed, int64(f)+777)
			durUS = frameUS + int64(r.IntN(int(frameUS/5)))-frameUS/10
			if durUS < frameUS/2 {
				durUS = frameUS / 2
			}
		}
		raw := driftedTicks(curUS, videoTB, tracks[0].DriftPPM)
		if tracks[0].WrapModulus > 0 {
			raw %= tracks[0].WrapModulus
		}
		durTicks, _ := timeline.FromStandard(durUS, videoTB)
		p := newPacket(cfg.Seed, tracks[0], uint32(f), raw, curUS, durTicks)
		clip.Packets = append(clip.Packets, p)
		curUS += durUS
	}

	// Audio: fixed 1024-sample packets on its own drifting clock.
	audioFrameUS := int64(1024) * 1_000_000 / 48000
	curUS = 0
	for a := 0; a < cfg.AudioPackets; a++ {
		raw := driftedTicks(curUS, audioTB, tracks[1].DriftPPM)
		if tracks[1].WrapModulus > 0 {
			raw %= tracks[1].WrapModulus
		}
		durTicks, _ := timeline.FromStandard(int64(audioFrameUS), audioTB)
		p := newPacket(cfg.Seed, tracks[1], uint32(a), raw, curUS, durTicks)
		clip.Packets = append(clip.Packets, p)
		curUS += int64(audioFrameUS)
	}

	// Subtitles spread across the span, then chapters, then metadata events.
	spanUS := curUS
	clip.Events = append(clip.Events, genSubtitles(cfg, metaND, spanUS)...)
	clip.Events = append(clip.Events, genChapters(cfg, metaND, spanUS)...)
	clip.Events = append(clip.Events, genMeta(cfg, metaND, spanUS)...)

	if err := assignFEC(clip); err != nil {
		return nil, err
	}
	if len(clip.Packets) > MaxPackets || len(clip.Events) > MaxEvents {
		return nil, timeline.GlobalError("fixture.generate", false,
			fmt.Errorf("%w: clip size", timeline.ErrLimit))
	}
	return clip, nil
}

func newPacket(seed int64, tr Track, seq uint32, raw, trueUS, durTicks int64) Packet {
	pay := make([]byte, tr.PayloadLen)
	deterministicPayload(pay, seed, tr.ID, seq)
	return Packet{
		Track:    tr.ID,
		Seq:      seq,
		RawTS:    raw,
		TrueUS:   trueUS,
		Duration: durTicks,
		Payload:  pay,
	}
}

// driftedTicks converts a true microsecond position into native ticks and
// applies linear clock drift (PPM) deterministically.
func driftedTicks(trueUS int64, tb timeline.Rational, ppm int) int64 {
	base, _ := timeline.FromStandard(trueUS, tb)
	if ppm == 0 {
		return base
	}
	drift := base * int64(ppm) / 1_000_000
	return base + drift
}

func mustTB(nd NumDen) timeline.Rational {
	r, err := timeline.NewRational(nd.Num, nd.Den)
	if err != nil {
		panic(err)
	}
	return r
}

// assignFEC groups media packets per track in blocks of FECGroupSize and
// appends one XOR parity packet per complete group. Tracks are processed in
// clip.Tracks order so the appended parity tail is byte-stable across reruns.
func assignFEC(clip *Clip) error {
	byTrack := map[uint32][]int{}
	for i, p := range clip.Packets {
		byTrack[p.Track] = append(byTrack[p.Track], i)
	}
	var parity []Packet
	for _, tr := range clip.Tracks {
		idxs := byTrack[tr.ID]
		for g, start := 0, 0; start < len(idxs); g, start = g+1, start+FECGroupSize {
			end := start + FECGroupSize
			if end > len(idxs) {
				break // incomplete tail group has no parity protection
			}
			for k := start; k < end; k++ {
				clip.Packets[idxs[k]].FECGroup = g
			}
			parity = append(parity, buildParity(clip, tr, g, idxs[start:end]))
		}
	}
	clip.Packets = append(clip.Packets, parity...)
	return nil
}

func findTrack(tracks []Track, id uint32) Track {
	for _, tr := range tracks {
		if tr.ID == id {
			return tr
		}
	}
	return Track{}
}

// buildParity XORs the group payloads; equal-length packets are guaranteed by
// the per-track fixed PayloadLen.
func buildParity(clip *Clip, tr Track, group int, idxs []int) Packet {
	n := tr.PayloadLen
	xor := make([]byte, n)
	for _, i := range idxs {
		for j, b := range clip.Packets[i].Payload {
			xor[j] ^= b
		}
	}
	first := clip.Packets[idxs[0]]
	return Packet{
		Track:    tr.ID,
		Seq:      uint32(0x80000000 | group),
		RawTS:    first.RawTS,
		TrueUS:   first.TrueUS,
		Duration: 0,
		Payload:  xor,
		FECGroup: group,
		IsParity: true,
	}
}
