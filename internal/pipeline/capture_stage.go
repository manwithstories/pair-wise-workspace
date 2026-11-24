package pipeline

import (
	"io"
	"os"

	"mediarig/internal/audio"
	"mediarig/internal/capture"
	"mediarig/internal/media"
	"mediarig/internal/txn"
)

// CaptureResult contains parsed tracks and the aligned rational clock map.
type CaptureResult struct {
	Assembler *capture.Assembler
	Clock     *capture.ClockAlignment
	Audio     *capture.Track
	Video     *capture.Track
	Records   int
}

// ReadCapture reads the length-prefixed input and enforces loss policy.
func ReadCapture(p *txn.Paths, params txn.Params) (*CaptureResult, error) {
	layout, err := audio.ParseLayout(params.Layout)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p.InputFile)
	if err != nil {
		return nil, media.InternalWrap(media.StageInput, err, "open input fixture")
	}
	defer f.Close()
	records, err := capture.ReadAll(io.Reader(f))
	if err != nil {
		return nil, err
	}
	registry := capture.DefaultPayloads()
	audioSpec := registry[96]
	audioSpec.ClockRate = params.Rate
	audioSpec.Channels = layout.Channels()
	registry[96] = audioSpec

	asm := capture.NewAssembler(registry)
	if err := asm.IngestRecords(records); err != nil {
		return nil, err
	}
	if err := asm.Finish(); err != nil {
		return nil, err
	}
	var audioTrack, videoTrack *capture.Track
	for _, tr := range asm.Tracks {
		switch tr.Spec.Format {
		case capture.PayloadL16:
			audioTrack = tr
		case capture.PayloadH264:
			videoTrack = tr
		}
	}
	if audioTrack == nil {
		return nil, media.Corrupt(media.StageRTP, "no L16 audio SSRC track")
	}
	if videoTrack == nil {
		return nil, media.Corrupt(media.StageRTP, "no H.264 video SSRC track")
	}
	if err := enforceLossPolicy(audioTrack, params.Rate); err != nil {
		return nil, err
	}
	clock, err := capture.AlignClocks(asm)
	if err != nil {
		return nil, err
	}
	return &CaptureResult{
		Assembler: asm, Clock: clock, Audio: audioTrack, Video: videoTrack,
		Records: len(records),
	}, nil
}

func enforceLossPolicy(tr *capture.Track, rate int) error {
	// Strictly more than 40 ms of consecutive absence is rejected.
	limit := media.MustRat(40, 1000)
	fallbackSamples := uint64(rate / 50)
	for _, g := range tr.Gaps {
		samples := tr.GapSamples(g)
		if samples <= 0 {
			samples = int(fallbackSamples) * g.Count
		}
		d := media.SamplesToSeconds(int64(samples), rate)
		if d.Cmp(limit) > 0 {
			err := media.Policy(media.StageLoss,
				"consecutive loss run of %d packets lasts %s s (>40ms)", g.Count, d.String())
			return media.WithSSRC(media.WithPacketSequence(err, uint16(g.MissingFrom)), tr.SSRC)
		}
	}
	if tr.Stats.ExpectedPackets > 0 {
		lost := tr.Stats.MissingPackets
		expected := tr.Stats.ExpectedPackets
		// Reject when lost/expected > 2/100.
		if lost*100 > expected*2 {
			return media.Policy(media.StageLoss, "packet loss %.2f%% exceeds 2%% policy (%d/%d)",
				100*float64(lost)/float64(expected), lost, expected)
		}
	}
	return nil
}

// TotalCounters sums recovery counters over all SSRC tracks.
func (c *CaptureResult) TotalCounters() (lost, reordered, duplicates int) {
	for _, tr := range c.Assembler.Tracks {
		lost += tr.Stats.MissingPackets
		reordered += tr.Stats.ReorderedPackets
		duplicates += tr.Stats.DuplicatePackets
	}
	return
}
