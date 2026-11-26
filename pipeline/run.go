package pipeline

import (
	"bytes"
	"errors"
	"fmt"

	"medcheck/boundary"
	"medcheck/clock"
	"medcheck/fanout"
	"medcheck/fault"
	"medcheck/index"
	"medcheck/ingest"
	"medcheck/internal/media"
	"medcheck/recover"
	"medcheck/subtitle"
)

var ValidCases = map[string]bool{"clean": true, "damaged": true, "recover": true}

// Run executes the complete deterministic inspection pipeline.
func Run(cfg Config) (*Report, error) {
	if !ValidCases[cfg.Case] {
		return nil, fmt.Errorf("unknown demo case %q", cfg.Case)
	}
	fm := fault.NewModel()
	fx := BuildFixture(cfg.Case, cfg.Seed)
	rep := &Report{Case: cfg.Case, Seed: cfg.Seed, Strict: cfg.Strict, HashOK: true,
		TimecodeBase: "25/1", TimecodeAligned: true}
	rep.InvalidDF, rep.MidnightWrap, rep.LTCVITCAligned = checkGeneratedTimecodes(fm)

	reps := fanout.Standard(1_000_000)
	bandwidth := fanout.NewController(reps, fm)
	_, _ = bandwidth.Observe(0, 5_000_000)
	if drop, _ := bandwidth.Observe(500, 700_000); drop {
		rep.BandwidthDrops = 1
	}

	layers := map[string]*recover.Layer{}
	for _, r := range reps {
		layer := recover.NewLayer(r.ID, fm)
		for segID := 0; segID < SegmentCount; segID++ {
			blocks := make([][]byte, BlocksPerSegment)
			for pos := 0; pos < BlocksPerSegment; pos++ {
				blocks[pos] = findPacket(fx.Ordered, uint64(segID*BlocksPerSegment+pos)).Payload
			}
			layer.AddSegment(recover.NewSegmentRepair(segID, 4, blocks))
		}
		layers[r.ID] = layer
	}

	w := ingest.NewWindow(64, fm)
	var delivered []ingest.Packet
	sink := func(rep fanout.Representation, pkt ingest.Packet) error {
		if layer := layers[rep.ID]; layer != nil {
			if sr := layer.Segments[int(pkt.Seq/BlocksPerSegment)]; sr != nil {
				sr.Receive(int(pkt.Seq%BlocksPerSegment), pkt.Payload, pkt.CRCValid)
			}
		}
		return nil
	}

	for _, frame := range fx.Frames {
		raw := ingest.EncodePacket(frame.Packet)
		if frame.CorruptCRC {
			// Byte 6 is a reserved header byte covered by CRC. Flipping it
			// invalidates CRC without changing camera/stream/sequence fields.
			raw[6] ^= 0x01
		}
		pkt, parseErr := ingest.ParsePacket(raw)
		if parseErr != nil {
			if errors.Is(parseErr, ingest.ErrBadPacketCRC) {
				rep.BadCRC++
				fm.Add(fault.Fault{Kind: fault.BadCRC, Camera: fx.Source.Camera, Stream: fx.Source.Stream,
					Span: fault.Span{StartSeq: pkt.Seq, EndSeq: pkt.Seq}, Message: parseErr.Error()})
			} else {
				return rep, parseErr
			}
		}
		beforeDup := w.Stats().Duplicates
		ready, err := w.Ingest(pkt)
		if err != nil {
			return rep, err
		}
		delivered = append(delivered, ready...)
		if w.Stats().Duplicates == beforeDup {
			if err := fanout.Fanout([]ingest.Packet{pkt}, reps, sink); err != nil {
				return rep, err
			}
		}
	}

	lossSpans := w.RecordLosses(fx.Source, TotalPackets-1)
	stats := w.Stats()
	rep.PacketsReceived, rep.Duplicates = stats.Received, stats.Duplicates
	rep.Reordered, rep.BurstLost = stats.Reordered, stats.BurstLost

	for _, r := range reps {
		layers[r.ID].RecoverAll()
	}
	primary := layers["1x"]
	for segID, positions := range primary.UnrecoverablePositions {
		for _, pos := range positions {
			seq := uint64(segID*BlocksPerSegment + pos)
			startMS := int64(segID*SegmentMillis + pos*(SegmentMillis/BlocksPerSegment))
			fm.Add(fault.Fault{Kind: fault.UnrecoveredBlock, Segment: segID,
				Span:    fault.Span{StartSeq: seq, EndSeq: seq, StartMS: startMS, EndMS: startMS + 500},
				Message: "precise unrecoverable block"})
		}
	}
	rep.Recovered = primary.RecoveredBlocks
	rep.DamagedSegments = len(primary.BadSegments)

	var recovered []ingest.Packet
	for _, span := range lossSpans {
		for seq := span.StartSeq; seq <= span.EndSeq; seq++ {
			segID, pos := int(seq/BlocksPerSegment), int(seq%BlocksPerSegment)
			if data := primary.Segments[segID].Data(pos); data != nil {
				recovered = append(recovered, ingest.Packet{Header: ingest.Header{
					Camera: fx.Source.Camera, Stream: fx.Source.Stream, Seq: seq, CRCValid: true,
				}, Payload: data})
			}
		}
	}
	more, err := w.AdvanceRecovered(recovered)
	if err != nil {
		return rep, err
	}
	delivered = append(delivered, more...)
	rep.Monotonic = isMonotonic(delivered)

	for segID := 0; segID < SegmentCount; segID++ {
		hash := primary.SegmentHashes[segID]
		rep.SegmentHashes = append(rep.SegmentHashes, SegmentHash{Segment: segID, SegmentSHA256: hash})
		if segID == 4 {
			rep.SegmentSHA256 = hash
		}
	}

	var units []media.AccessUnit
	var auBad []media.HeaderError
	if err := media.ScanReader(bytes.NewReader(fx.AUBytes), func(u media.AccessUnit, he *media.HeaderError) error {
		if he != nil {
			auBad = append(auBad, *he)
		} else {
			units = append(units, u)
		}
		return nil
	}); err != nil {
		return rep, err
	}
	for _, bad := range auBad {
		fm.Add(fault.Fault{Kind: bad.Kind, Message: bad.Error()})
	}
	idx, gopErrs := index.BuildGOP(units, primary.SegmentHashes, cfg.Seed)
	for _, gerr := range gopErrs {
		fm.Add(fault.Fault{Kind: fault.BadSegment, Message: gerr.Error()})
	}
	for i := range idx.Segments {
		if i+1 < len(idx.Segments) {
			idx.Segments[i].EndMS = idx.Segments[i+1].StartMS
		} else {
			idx.Segments[i].EndMS = idx.Segments[i].StartMS + SegmentMillis
		}
	}
	rep.KeyframesFound, rep.KeyframesExpected = idx.KeyframesFound, idx.KeyframesExpected

	srtGot, srtBytes, err := subtitle.RoundTrip(fx.CleanCues, subtitle.FormatSRT)
	if err != nil {
		return rep, err
	}
	vttGot, vttBytes, err := subtitle.RoundTrip(fx.CleanCues, subtitle.FormatVTT)
	if err != nil {
		return rep, err
	}
	rep.CuesTotal = len(fx.CleanCues)
	if subtitle.Equal(fx.CleanCues, srtGot) && subtitle.Equal(fx.CleanCues, vttGot) {
		rep.CuesOK = rep.CuesTotal
	}
	rep.RoundtripPercent = float64(rep.CuesOK) / float64(rep.CuesTotal) * 100
	for _, cue := range fx.CleanCues {
		for _, ms := range []int64{cue.StartMS, cue.EndMS} {
			frame := clock.Rate25.MillisToFrames(ms)
			if clock.Rate25.FramesToMillis(frame) != ms {
				rep.TimecodeAligned = false
				fm.Add(fault.Fault{Kind: fault.InvalidTimecode, SourceLine: cue.SourceLine,
					Span: fault.Span{StartMS: ms, EndMS: ms}, Message: "subtitle boundary is not on a 25 fps frame"})
			}
		}
	}

	cleanSRT, err := subtitle.Encode(fx.CleanCues, subtitle.FormatSRT)
	if err != nil {
		return rep, err
	}
	subtitleInput := cleanSRT
	if cfg.Case == "damaged" {
		subtitleInput = append(append([]byte{}, cleanSRT...), []byte(fx.MalformedSRT)...)
	}
	doc, err := subtitle.Parse(bytes.NewReader(subtitleInput), subtitle.FormatSRT)
	if err != nil {
		return rep, err
	}
	rep.IsolatedIssues = len(doc.Issues)
	for _, issue := range doc.Issues {
		fm.Add(fault.Fault{Kind: issue.Kind, SourceLine: issue.SourceLine,
			Span: fault.Span{StartMS: issue.StartMS, EndMS: issue.EndMS}, Message: issue.Message})
	}

	layerStates := make([]boundary.LayerState, 0, len(reps))
	for _, r := range reps {
		l := layers[r.ID]
		layerStates = append(layerStates, boundary.LayerState{Name: r.ID, Hashes: l.SegmentHashes, BadSegments: l.BadSegments})
	}
	br := boundary.Check(idx, doc, layerStates, fm)
	rep.ConflictCount = br.ConflictCount
	rep.CuttableBoundaries = len(br.CuttableBoundaries)
	rep.HashOK = br.HashOK

	if cfg.WorkDir != "" {
		resumed, err := persistOutputs(cfg.WorkDir, cfg.Case, idx, srtBytes, vttBytes)
		if err != nil {
			return rep, &IOError{Op: "persist outputs", Err: err}
		}
		rep.IndexResumed = resumed
	}
	return rep, nil
}

func isMonotonic(packets []ingest.Packet) bool {
	if len(packets) == 0 {
		return false
	}
	for i := 1; i < len(packets); i++ {
		if packets[i].Seq != packets[i-1].Seq+1 {
			return false
		}
	}
	return true
}
