package pipeline

import (
	"fmt"

	"avpipe/internal/align"
	"avpipe/internal/fixture"
	"avpipe/internal/index"
	"avpipe/internal/media"
	"avpipe/internal/ts"
)

// BuildPlan deterministically generates and analyzes source material.
func BuildPlan(opt Options) (*Report, []fixture.Source, error) {
	if opt.Seed == 0 {
		opt.Seed = 23
	}
	if opt.Case == "" {
		opt.Case = string(fixture.CaseClean)
	}
	if len(opt.Bitrates) == 0 {
		opt.Bitrates = []int64{600000, 1200000}
	}
	if opt.Segments == 0 {
		opt.Segments = fixture.Slots
	}
	if len(opt.Bitrates) < 2 {
		return nil, nil, media.NewError(media.CodeConfig, -1, "at least two bitrates are required")
	}
	cfgHash, err := ConfigHash(opt)
	if err != nil {
		return nil, nil, err
	}
	sources, err := fixture.Generate(toFixtureConfig(opt))
	if err != nil {
		return nil, nil, media.WrapError(media.CodeConfig, -1, err, "generate fixture")
	}
	rep := &Report{
		Case: opt.Case, Out: opt.Out, ParserVersion: media.ParserVersion,
		SourceHash: fixture.SourceHash(sources), ConfigHash: cfgHash,
		TotalSlots: opt.Segments, FirstDesync: -1, ClockEpochs: map[string][]media.Epoch{},
	}

	indexes := make([]*index.Index, 0, len(sources))
	for _, src := range sources {
		a, err := analyzeSource(opt, src)
		if err != nil {
			return rep, sources, err
		}
		rep.Analyses = append(rep.Analyses, *a)
		indexes = append(indexes, a.Index)
		rep.Packets += a.Stats.PacketCount
		rep.IncompleteTail += a.Stats.IncompleteTail
		rep.CCGap += len(a.Index.CCGaps)
		rep.PSICRCReject += a.PSI.CRCRejectCount()
		rep.ClockEpochs[src.Rendition] = a.Clock.Epochs
		if a.Stats.FirstDesyncOffset >= 0 && (rep.FirstDesync < 0 || a.Stats.FirstDesyncOffset < rep.FirstDesync) {
			rep.FirstDesync = a.Stats.FirstDesyncOffset
		}
	}
	rep.Recovery = &RecoveryRecord{}
	for i := range rep.Analyses {
		a := &rep.Analyses[i]
		for _, seq := range a.Index.MarkMissingSlots() {
			rep.Recovery.MissingIDRSlots = append(rep.Recovery.MissingIDRSlots, MissingIDRSlot{
				Rendition: a.Source.Rendition, Seq: seq, Detail: "expected AVC IDR anchor absent",
			})
		}
		rep.Recovery.PSIRejections = append(rep.Recovery.PSIRejections, a.PSI.Rejections...)
		rep.Recovery.CCGaps = append(rep.Recovery.CCGaps, a.Index.CCGaps...)
	}
	found, expected := index.ExpectedHits(indexes)
	rep.KeyframeFound = found
	rep.KeyframeExpected = expected
	ladder, err := align.Align(indexes, opt.Segments)
	if err != nil {
		return rep, sources, err
	}
	rep.Ladder = ladder
	rep.AlignedSlots = ladder.AlignedSlots
	rep.GapSlots = ladder.GapSlots
	for _, s := range ladder.Slots {
		if s.Gap {
			rep.Recovery.GapSlots = append(rep.Recovery.GapSlots, s.Seq)
		}
	}
	return rep, sources, nil
}

func analyzeSource(opt Options, src fixture.Source) (*Analysis, error) {
	packets, stats, err := ts.ParseStream(src.Data)
	if err != nil {
		// Sync failure is fatal; short tail is represented in stats when
		// ParseStream can still return packets.
		if stats.PacketCount == 0 {
			return nil, err
		}
	}
	psiState := ts.NewPSIState()
	assembler := ts.NewSectionAssembler()
	clock := media.NewClock()
	for _, p := range packets {
		if p.HasPCR {
			_, _, _ = clock.FeedPCR(p.PCR27, int64(p.Offset), p.DiscontinuityIndicator)
		}
		handlePSI := p.PID == ts.PATPID
		if psiState.PAT != nil && p.PID == psiState.PAT.PMTPID {
			handlePSI = true
		}
		if handlePSI {
			for _, tab := range assembler.HandlePacket(p, psiState) {
				psiState.Apply(tab)
			}
		}
	}
	if err := clock.RequireAnchor(); err != nil {
		return nil, err
	}
	var videoPID, audioPID uint16 = fixture.VideoPID, fixture.AudioPID
	if psiState.PAT != nil {
		if pmt := psiState.PMTs[psiState.PAT.PMTPID]; pmt != nil {
			if pmt.VideoPID != 0 {
				videoPID = pmt.VideoPID
			}
			if pmt.AudioPID != 0 {
				audioPID = pmt.AudioPID
			}
		}
	}
	idx, err := index.Build(packets, index.BuildConfig{
		Rendition: src.Rendition, Bitrate: src.Bitrate,
		VideoPID: videoPID, AudioPID: audioPID,
		ExpectedSlots: opt.Segments, PacketsPerSlot: fixture.PacketsPerSlot,
	})
	if err != nil {
		return nil, err
	}
	return &Analysis{
		Source: src, Packets: packets, Stats: stats, PSI: psiState, Clock: clock,
		Index: idx, VideoPID: videoPID, AudioPID: audioPID,
	}, nil
}

func AnalysisByName(rep *Report, name string) *Analysis {
	for i := range rep.Analyses {
		if rep.Analyses[i].Source.Rendition == name {
			return &rep.Analyses[i]
		}
	}
	return nil
}

// SegmentPayload returns only complete framed TS packets belonging to a slot.
// An incomplete tail is never copied into a segment.
func SegmentPayload(a *Analysis, seq int) ([]byte, int, int) {
	var out []byte
	first, end := -1, -1
	for _, p := range a.Packets {
		frameSeq := (p.Offset / ts.PacketSize) / fixture.PacketsPerSlot
		if frameSeq != seq {
			continue
		}
		if first < 0 {
			first = p.Offset
		}
		out = append(out, a.Source.Data[p.Offset:p.Offset+ts.PacketSize]...)
		end = p.Offset + ts.PacketSize
	}
	return out, first, end
}

func MustConfigHash(opt Options) string {
	h, err := ConfigHash(opt)
	if err != nil {
		return fmt.Sprintf("error:%v", err)
	}
	return h
}
