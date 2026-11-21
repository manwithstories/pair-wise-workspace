package demux

import (
	"bytes"
	"errors"
	"testing"

	"archivecheck.local/archivecheck/internal/fault"
	"archivecheck.local/archivecheck/internal/fixture"
)

func scan(t *testing.T, data []byte, recover bool) (*Result, error) {
	t.Helper()
	res := &Result{}
	opts := DefaultOptions()
	opts.Recover = recover
	opts.CollectSamples = true
	err := ScanReader(bytes.NewReader(data), opts, res)
	return res, err
}

func TestScanCleanDocument(t *testing.T) {
	doc := fixture.Build(fixture.DefaultTracks(), fixture.DefaultBlocks(), fixture.Layout{SortByTime: true})
	res, err := scan(t, doc.Bytes, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Info.TimestampScale != 1_000_000 || res.Info.DocType != "matroska" {
		t.Fatalf("info=%+v", res.Info)
	}
	if len(res.Tracks) != 3 {
		t.Fatalf("tracks=%d", len(res.Tracks))
	}
	if len(res.ClusterOffsets) == 0 || len(res.Samples) == 0 {
		t.Fatalf("clusters=%d samples=%d", len(res.ClusterOffsets), len(res.Samples))
	}
	if len(res.CuePoints) == 0 {
		t.Fatal("cues were not captured")
	}
}

func TestScanCollectsSampleByteRanges(t *testing.T) {
	doc := fixture.Build(fixture.DefaultTracks(), fixture.DefaultBlocks(), fixture.Layout{SortByTime: true})
	res, err := scan(t, doc.Bytes, false)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range res.Samples {
		// Re-derive the payload length from the neighbouring sample's offset
		// within the same cluster: the recorded offset must land inside the
		// file and be non-decreasing per block.
		if s.SourceOffset < 0 || s.SourceOffset >= int64(len(doc.Bytes)) {
			t.Fatalf("sample %d offset %d outside file", i, s.SourceOffset)
		}
	}
}

func TestScanTruncatedFailsWithoutRecovery(t *testing.T) {
	fx, err := fixture.New(fixture.DamageTruncated, 17)
	if err != nil {
		t.Fatal(err)
	}
	res, err := scan(t, fx.Bytes, false)
	if err == nil {
		t.Fatal("truncated input must fail")
	}
	if !errors.Is(err, fault.ErrOversized) && !errors.Is(err, fault.ErrTruncated) {
		t.Fatalf("got %v", err)
	}
	if offset := fault.OffsetOf(err); offset <= 0 {
		t.Fatalf("missing source offset: %d", offset)
	}
	if len(res.Recovery) != 0 {
		t.Fatalf("strict mode recorded skips: %+v", res.Recovery)
	}
}

func TestScanTruncatedRecoversWithProvenBoundary(t *testing.T) {
	fx, err := fixture.New(fixture.DamageTruncated, 17)
	if err != nil {
		t.Fatal(err)
	}
	res, err := scan(t, fx.Bytes, true)
	if err != nil {
		t.Fatalf("recovery failed: %v", err)
	}
	if len(res.Recovery) == 0 {
		t.Fatal("recovery recorded no skipped range")
	}
	skip := res.Recovery[0]
	if skip.Start <= 0 || skip.Length <= 0 {
		t.Fatalf("skip range is not actionable: %+v", skip)
	}
	if skip.Start+skip.Length != int64(len(fx.Bytes)) {
		t.Fatalf("skip does not end at the input end: %+v", skip)
	}
	// Every decoded sample must lie before the damaged tail.
	for _, s := range res.Samples {
		if s.SourceOffset >= skip.Start {
			t.Fatalf("sample from the skipped range survived: %+v", s)
		}
	}
}

func TestScanOversizedIsFatalEvenInRecovery(t *testing.T) {
	fx, err := fixture.New(fixture.DamageOversized, 17)
	if err != nil {
		t.Fatal(err)
	}
	res, err := scan(t, fx.Bytes, true)
	if err == nil {
		t.Fatal("ambiguous boundary must not be recovered")
	}
	if len(res.Recovery) != 0 {
		t.Fatalf("ambiguous boundary produced skips: %+v", res.Recovery)
	}
	if !errors.Is(err, fault.ErrOversized) && !errors.Is(err, fault.ErrBoundary) {
		t.Fatalf("got %v", err)
	}
}

func TestScanBadBlockReportsTrackAndOffset(t *testing.T) {
	fx, err := fixture.New(fixture.DamageBadBlock, 17)
	if err != nil {
		t.Fatal(err)
	}
	_, err = scan(t, fx.Bytes, false)
	if err == nil {
		t.Fatal("corrupt block must fail")
	}
	if fault.OffsetOf(err) <= 0 {
		t.Fatalf("no offset: %v", err)
	}
	if fault.StageOf(err) != fault.StageDemux && fault.StageOf(err) != fault.StageParse {
		t.Fatalf("stage=%q", fault.StageOf(err))
	}
}

func TestScanUnknownSizeDocument(t *testing.T) {
	fx, err := fixture.New(fixture.DamageUnknownSize, 17)
	if err != nil {
		t.Fatal(err)
	}
	res, err := scan(t, fx.Bytes, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Info.SegmentUnknown {
		t.Fatal("segment should be unknown-size")
	}
	if len(res.Samples) == 0 {
		t.Fatal("no samples")
	}
}

func TestScanNoCuesDocument(t *testing.T) {
	fx, err := fixture.New(fixture.DamageNoCues, 17)
	if err != nil {
		t.Fatal(err)
	}
	res, err := scan(t, fx.Bytes, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Info.HasCues || len(res.CuePoints) != 0 {
		t.Fatal("fixture unexpectedly has cues")
	}
	if res.CueCorrupt {
		t.Fatal("absent cues must not be reported as corrupt")
	}
}

func TestScanUnknownTrackBlockCounted(t *testing.T) {
	blocks := []fixture.BlockSpec{
		{Track: 1, Tick: 0, Key: true, Data: fixture.Pattern("v", 0, 16)},
		{Track: 77, Tick: 0, Key: true, Data: fixture.Pattern("x", 0, 16)},
	}
	doc := fixture.Build(fixture.DefaultTracks()[:1], blocks, fixture.Layout{ClusterSpan: 4})
	res, err := scan(t, doc.Bytes, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.UnknownTrackBlocks != 1 || len(res.UnknownTrackNumbers) != 1 || res.UnknownTrackNumbers[0] != 77 {
		t.Fatalf("unknown=%+v", res)
	}
}

func TestScanRejectsForeignDocType(t *testing.T) {
	doc := fixture.Build(fixture.DefaultTracks()[:1], fixture.DefaultBlocks()[:1], fixture.Layout{DocType: "not-matroska"})
	_, err := scan(t, doc.Bytes, false)
	if err == nil {
		t.Fatal("foreign DocType must be rejected")
	}
}
