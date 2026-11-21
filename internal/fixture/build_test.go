package fixture

import (
	"bytes"
	"testing"

	"archivecheck.local/archivecheck/internal/demux"
)

func scanDoc(t *testing.T, doc Document, opts demux.Options) (*demux.Result, []demux.Sample) {
	t.Helper()
	res := &demux.Result{}
	var samples []demux.Sample
	opts.OnSample = func(s demux.Sample) error {
		cp := s
		cp.Data = append([]byte(nil), s.Data...)
		samples = append(samples, cp)
		return nil
	}
	r := bytes.NewReader(doc.Bytes)
	if err := demux.ScanReader(r, opts, res); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return res, samples
}

func TestBuildScansCleanly(t *testing.T) {
	doc := Build(DefaultTracks(), DefaultBlocks(), Layout{SortByTime: true, ClusterSpan: 5})
	res, samples := scanDoc(t, doc, demux.DefaultOptions())
	if res.Info.DocType != "matroska" {
		t.Fatalf("doctype=%q", res.Info.DocType)
	}
	if res.Info.TimestampScale != 1_000_000 {
		t.Fatalf("scale=%d", res.Info.TimestampScale)
	}
	if len(res.Tracks) != 3 {
		t.Fatalf("tracks=%d", len(res.Tracks))
	}
	if len(samples) != 12*2+3 {
		t.Fatalf("samples=%d", len(samples))
	}
	if !res.Info.HasCues || res.Info.CueEntries == 0 {
		t.Fatalf("cues not found: %+v", res.Info)
	}
	for _, s := range samples {
		if s.SourceOffset < 0 || s.SourceOffset+int64(len(s.Data)) > int64(len(doc.Bytes)) {
			t.Fatalf("sample offset outside file: %+v", s)
		}
		if !bytes.Equal(doc.Bytes[s.SourceOffset:s.SourceOffset+int64(len(s.Data))], s.Data) {
			t.Fatalf("sample data is not a substring of the source")
		}
	}
}

func TestBuildUnknownSegmentAndCluster(t *testing.T) {
	doc := Build(DefaultTracks(), DefaultBlocks(), Layout{
		SortByTime: true, ClusterSpan: 4, UnknownSegment: true, UnknownCluster: true,
	})
	res, samples := scanDoc(t, doc, demux.DefaultOptions())
	if !res.Info.SegmentUnknown {
		t.Fatal("segment should be unknown-size")
	}
	if len(samples) != 27 {
		t.Fatalf("samples=%d", len(samples))
	}
}

func TestBuildNoCues(t *testing.T) {
	doc := Build(DefaultTracks(), DefaultBlocks(), Layout{SortByTime: true, NoCues: true})
	res, _ := scanDoc(t, doc, demux.DefaultOptions())
	if res.Info.HasCues || res.Info.CueEntries != 0 {
		t.Fatalf("cues should be absent: %+v", res.Info)
	}
}

func TestBuildOutOfOrderBlocks(t *testing.T) {
	blocks := []BlockSpec{
		{Track: 1, Tick: 80, Key: true, Data: Pattern("v", 8, 16)},
		{Track: 1, Tick: 0, Key: true, Data: Pattern("v", 0, 16)},
		{Track: 1, Tick: 40, Key: false, Data: Pattern("v", 4, 16)},
	}
	doc := Build(DefaultTracks()[:1], blocks, Layout{ClusterSpan: 8})
	res, samples := scanDoc(t, doc, demux.DefaultOptions())
	if len(samples) != 3 {
		t.Fatalf("samples=%d", len(samples))
	}
	if samples[0].Timestamp != 80 || samples[1].Timestamp != 0 {
		t.Fatalf("source order not preserved: %d,%d", samples[0].Timestamp, samples[1].Timestamp)
	}
	_ = res
}

func TestUnknownTrackBlocksCounted(t *testing.T) {
	blocks := []BlockSpec{
		{Track: 1, Tick: 0, Key: true, Data: Pattern("v", 0, 16)},
		{Track: 99, Tick: 0, Key: true, Data: Pattern("x", 0, 16)},
	}
	doc := Build(DefaultTracks()[:1], blocks, Layout{ClusterSpan: 8})
	res, samples := scanDoc(t, doc, demux.DefaultOptions())
	if len(samples) != 1 {
		t.Fatalf("declared samples=%d", len(samples))
	}
	if res.UnknownTrackBlocks != 1 || len(res.UnknownTrackNumbers) != 1 || res.UnknownTrackNumbers[0] != 99 {
		t.Fatalf("unknown stats: %+v", res)
	}
}

func TestLacedAudioSplitsIntoSamples(t *testing.T) {
	blocks := []BlockSpec{
		{Track: 1, Tick: 0, Key: true, Data: Pattern("v", 0, 32)},
		{Track: 2, Tick: 0, Key: true, Laced: 3, Data: Pattern("a", 0, 48)},
	}
	doc := Build(DefaultTracks()[:2], blocks, Layout{ClusterSpan: 8})
	_, samples := scanDoc(t, doc, demux.DefaultOptions())
	if len(samples) != 4 {
		t.Fatalf("samples=%d", len(samples))
	}
	if samples[2].FrameIndex != 1 || !samples[2].Laced {
		t.Fatalf("lacing not reported: %+v", samples[2])
	}
}
