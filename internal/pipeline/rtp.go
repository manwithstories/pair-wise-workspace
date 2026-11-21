package pipeline

import (
	"fmt"

	"mediad/internal/clock"
	"mediad/internal/fixture"
	"mediad/internal/manifest"
	"mediad/internal/model"
	"mediad/internal/rtp"
)

func runRTP(opt DemoOptions, res *Result) (rtp.LossReport, rtp.Stats, int, *model.Track, error) {
	const clockRate = 90000
	const samples = 120
	const payloadBytes = 800
	// Start the 32-bit media clock just before its modulus so the stream
	// genuinely wraps once during the run.
	const startTS = uint32(0xFFFFFFFF - 60*3000)
	pkts := fixture.Packetise(opt.Seed, 1, samples, payloadBytes, clockRate, rtpTicksPerFrame, 65530, startTS)
	params := fixture.FaultParams{
		DuplicateAt: []int{7, 23, 41},
		ReorderAt:   []int{12, 55, 77},
		BurstStart:  90,
		BurstLen:    4, // short: bridged from intact neighbours
		Burst2Start: 100,
		Burst2Len:   12, // long: reported as unrecoverable
	}
	arrivals, faults := fixture.ApplyFaults(pkts, opt.Faults, params)
	cfg := rtp.DefaultConfig()
	cfg.ClockRate = clockRate
	cfg.InitialWindow = 32
	cfg.MinWindow = 8
	cfg.MaxWindow = 256
	recv, err := rtp.NewReceiver(cfg)
	if err != nil {
		return rtp.LossReport{}, rtp.Stats{}, 0, nil, err
	}
	var delivered []rtp.Packet
	for i, p := range arrivals {
		if n := len(opt.BandwidthMbps); n > 0 {
			recv.SetBandwidth(opt.BandwidthMbps[i*n/len(arrivals)%n])
		}
		got, err := recv.Push(p)
		if err != nil {
			return rtp.LossReport{}, rtp.Stats{}, 0, nil, err
		}
		delivered = append(delivered, got...)
	}
	delivered = append(delivered, recv.Flush()...)
	stats := recv.Stats()
	rep := rtp.ClassifyLoss(stats, delivered, opt.MaxBurst)
	tr, err := recv.Track(1, delivered)
	if err != nil {
		return rtp.LossReport{}, rtp.Stats{}, 0, nil, err
	}
	tr.Source = "rtp"
	if err := rtp.DamagedRanges(&tr, rep); err != nil {
		return rtp.LossReport{}, rtp.Stats{}, 0, nil, err
	}
	if len(rep.Gaps) > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf(
			"rtp: %d gap(s), %d recovered packet(s), %d unrecoverable",
			len(rep.Gaps), rep.RecoveredSamples, rep.UnrecoverableSamples))
	}
	if n := len(stats.WindowShifts); n > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("rtp: reorder window adapted %d time(s) to bandwidth", n))
	}
	return rep, stats, faults, &tr, nil
}

// targetSpecs is the delivery manifest, authored from the spec rather than
// from probed media. Targets carry the spec's 2% bitrate safety margin.
func targetSpecs() []manifest.TrackTarget {
	spec := func(id uint32, frames, delta, timescale int64, sampleBytes float64) manifest.TrackTarget {
		dur, _ := clock.New(frames*delta, timescale)
		// A sample carries sampleBytes and one frame lasts delta/timescale
		// seconds, so the rate is sampleBytes*8*timescale/delta.
		rate := sampleBytes * 8 * float64(timescale) / float64(delta) / 1000
		return manifest.TrackTarget{
			TrackID: id, Start: clock.Zero(), End: dur,
			TargetBitrateKbps: rate / (1 - headroomFraction),
		}
	}
	return []manifest.TrackTarget{
		spec(1, videoFrames, videoDelta, videoTimescale, videoBytes),
		spec(2, audioFrames, audioDelta, audioTimescale, audioBytes),
		spec(3, videoFrames, videoDelta, videoTimescale, mxfEssenceBytes),
	}
}

// WriteBadCropFixture writes an MP4 whose clean aperture exceeds the coded
// frame. Probing it must fail with the track ID and time range, never
// silently truncate.
