package demo

import (
	"fmt"

	"medialab/internal/clock"
	"medialab/internal/model"
	"medialab/internal/report"
	"medialab/internal/rtp"
	"medialab/internal/sample"
	"medialab/internal/sync"
)

type burstOutcome struct {
	payloads    [][]byte
	packets     []*rtp.InPacket
	recovered   [][]byte
	states      []string
	refs        []model.SegmentRef
	segments    []report.SegmentResult
	engine      *rtp.Engine
	syncReport  *sync.Report
	unwrappedTS []int64
}

func runBurstScenario(seed int64, burstLoss int) (*burstOutcome, error) {
	payloads := sample.Segments(seed, sample.SegmentCount)
	packets, _, err := sample.BuildRTPStream(payloads)
	if err != nil {
		return nil, err
	}

	const start = 2
	lost := make(map[int]bool)
	lostSeqs := make([]uint16, 0, burstLoss)
	for k := 0; k < burstLoss; k++ {
		i := start + k
		if i >= len(packets) {
			return nil, fmt.Errorf("burst loss %d exceeds bounded scenario", burstLoss)
		}
		lost[i] = true
		lostSeqs = append(lostSeqs, packets[i].P.SequenceNumber)
	}

	available := make(map[uint16]*rtp.InPacket, len(packets))
	engine := rtp.NewEngine(80)
	for i, p := range packets {
		engine.Cache.Store(int64(i)*20, p)
		if !lost[i] {
			available[p.P.SequenceNumber] = p
		}
	}

	partner := firstAvailablePartner(lost)
	for k := 0; k < burstLoss; k++ {
		i := start + k
		group, gerr := rtp.BuildXORGroup(fmt.Sprintf("xor-%02d", i), packets[i], packets[partner])
		if gerr != nil {
			return nil, gerr
		}
		if aerr := engine.FEC.Add(group); aerr != nil {
			return nil, aerr
		}
	}
	recoveryResults := engine.RecoverLost(220, lostSeqs, available)
	stateBySeq := make(map[uint16]string, len(recoveryResults))
	for _, rr := range recoveryResults {
		stateBySeq[rr.Seq] = string(rr.State)
	}

	out := &burstOutcome{
		payloads:  payloads,
		packets:   packets,
		recovered: make([][]byte, len(payloads)),
		states:    make([]string, len(payloads)),
		refs:      make([]model.SegmentRef, len(payloads)),
		engine:    engine,
	}
	for i, p := range packets {
		if rp, ok := available[p.P.SequenceNumber]; ok {
			out.recovered[i] = rp.P.Payload
		}
		state := model.StateOriginal
		if lost[i] {
			out.states[i] = stateBySeq[p.P.SequenceNumber]
			state = model.RecoveryState(out.states[i])
		} else {
			out.states[i] = string(model.StateOriginal)
		}
		out.refs[i] = model.SegmentRef{
			Index: i, TrackID: p.TrackID, SourceOff: p.RawOffset, State: state,
		}
	}
	out.segments = report.VerifySegments(payloads, out.recovered, out.states)
	out.syncReport, out.unwrappedTS = alignBurst(packets, out.segments)
	return out, nil
}

func firstAvailablePartner(lost map[int]bool) int {
	for i := 0; i < sample.SegmentCount; i++ {
		if !lost[i] {
			return i
		}
	}
	return 0
}

func alignBurst(packets []*rtp.InPacket, segments []report.SegmentResult) (*sync.Report, []int64) {
	unwrapper := &rtp.TimestampUnwrapper{}
	unwrapped := make([]int64, len(packets))
	for i, p := range packets {
		unwrapped[i] = unwrapper.Unwrap(p.P.Timestamp)
	}
	base := unwrapped[0]
	keyframes := map[int]bool{0: true, 3: true, 6: true, 9: true}
	video := make([]sync.Frame, len(packets))
	audio := make([]sync.Frame, len(packets))
	for i, p := range packets {
		pts := clock.NewTime(unwrapped[i]-base, 90000)
		matched := i < len(segments) && segments[i].Match
		video[i] = sync.Frame{
			Segment: i, TrackID: p.TrackID, Kind: "video", PTS: pts, DurationMS: 33,
			Keyframe: keyframes[i], Verified: matched, Present: true, State: "original",
		}
		audioMS := pts.Milliseconds() - 12
		if i == 7 {
			audioMS -= 60 // one correctable drift spike.
		}
		a := sync.Frame{
			Segment: i, TrackID: 2, Kind: "audio",
			PTS: clock.FromMilliseconds(audioMS), DurationMS: 20,
			Verified: true, Present: true, State: "original",
		}
		if i == 5 {
			a.Present = false
			a.State = "silence_concealed"
		}
		audio[i] = a
	}
	return sync.Align(video, audio), unwrapped
}
