package pipeline

import (
	"path/filepath"
	"testing"

	"fieldcut/internal/parse"
	"fieldcut/internal/timeline"
)

func testVideoTrack(t *testing.T, n int, every int) *parse.TrackInfo {
	clock, err := timeline.NewClock(1, timeline.KindVideo, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	ti := &parse.TrackInfo{TrackID: 1, Kind: "video", Codec: "avc1", Clock: clock}
	off := uint64(1000)
	for i := 0; i < n; i++ {
		size := uint32(50 + i)
		sync := i%every == 0
		ti.Samples = append(ti.Samples, parse.Sample{
			Index: uint32(i + 1), Offset: off, Size: size,
			TimeNs: int64(i) * 1_000_000_000, DurationNs: 1_000_000_000,
			RawTick: uint64(i * 1000), Sync: sync, SyncKnown: true,
		})
		off += uint64(size)
	}
	return ti
}

func TestSplitAlignsToSyncPoints(t *testing.T) {
	ti := testVideoTrack(t, 10, 2) // 每 2s 一个随机访问点
	cfg := CutConfig{TargetNs: 3_000_000_000, MinNs: 500_000_000,
		SlopNs: 1_500_000_000}
	segs := NewSegmenter(cfg, ti).Split(ti)
	if len(segs) < 2 {
		t.Fatalf("expected multiple segments, got %d", len(segs))
	}
	for _, s := range segs {
		if !s.SafeCut {
			t.Fatalf("all cuts must align to known sync: %+v", s)
		}
		if len(s.ByteRanges) == 0 {
			t.Fatal("segment must carry aligned byte ranges")
		}
	}
	// 切点必须落在某个同步样本时间上。
	syncTimes := map[int64]bool{}
	for _, sm := range ti.Samples {
		if sm.Sync {
			syncTimes[sm.TimeNs] = true
		}
	}
	for _, s := range segs[1:] {
		if !syncTimes[s.StartNs] {
			t.Fatalf("segment start %d not a sync point", s.StartNs)
		}
	}
}

func TestSplitUnsafeWhenNoSync(t *testing.T) {
	ti := testVideoTrack(t, 6, 2)
	for i := range ti.Samples {
		ti.Samples[i].SyncKnown = false
		ti.Samples[i].Sync = false
	}
	segs := NewSegmenter(DefaultCutConfig(), ti).Split(ti)
	if len(segs) != 1 || !segs[0].Degraded || segs[0].SafeCut {
		t.Fatalf("unknown sync must yield one degraded, unsafe segment: %+v", segs)
	}
}

func TestAttachEventsClipsAtBoundary(t *testing.T) {
	segs := []*Segment{
		{ID: "a", StartNs: 0, EndNs: 2_000_000_000},
		{ID: "b", StartNs: 2_000_000_000, EndNs: 4_000_000_000},
	}
	events := []parse.Event{
		{Kind: parse.EvSubtitle, TrackID: 9, StartNs: 1_500_000_000,
			EndNs: 2_500_000_000, Text: "cross"}, // 跨越 a/b 边界
		{Kind: parse.EvEmsg, StartNs: 3_000_000_000, EndNs: 3_100_000_000},
	}
	assigned := AttachEvents(segs, events)
	if len(assigned) != 3 {
		t.Fatalf("cross-boundary event must attach to both segments, got %d", len(assigned))
	}
	for _, a := range assigned {
		if a.Event.Text == "cross" {
			if a.Event.StartNs < 0 || a.Event.EndNs > 4_000_000_000 {
				t.Fatal("event not clipped to segment range")
			}
		}
	}
}

func TestMapAudioWindow(t *testing.T) {
	clock, _ := timeline.NewClock(2, timeline.KindAudio, 48000, 0)
	audio := &parse.TrackInfo{TrackID: 2, Kind: "audio", Clock: clock}
	for i := 0; i < 8; i++ {
		audio.Samples = append(audio.Samples, parse.Sample{
			Index: uint32(i + 1), TimeNs: int64(i) * 500_000_000,
			DurationNs: 500_000_000})
	}
	segs := []*Segment{
		{ID: "a", StartNs: 0, EndNs: 2_000_000_000},
		{ID: "b", StartNs: 2_000_000_000, EndNs: 4_000_000_000},
	}
	m := MapAudio(segs, audio)
	if len(m["a"]) == 0 || m["a"][0][0] != 1 || m["b"][0][0] < 4 {
		t.Fatalf("audio window mapping wrong: %+v", m)
	}
}

func TestSummaryEvaluateAndManifestRerun(t *testing.T) {
	ti := testVideoTrack(t, 12, 2)
	segs := NewSegmenter(DefaultCutConfig(), ti).Split(ti)
	for i, s := range segs {
		s.ID = segID(i)
		s.SHA256 = hashStub(i)
	}
	rep := &parse.Report{SkippedBoxes: 1}
	rep.Errors = append(rep.Errors, &parse.Error{Level: parse.LevelRecoverable,
		BoxType: "stts"})
	sum := BuildSummary(segs, rep, 300, 99, 100, 2048)
	if sum.RoundtripRate < 0.98 || sum.Segments < 4 {
		t.Fatalf("summary metrics wrong: %+v", sum)
	}
	if code := sum.Evaluate(DefaultThresholds()); code != 0 {
		t.Fatalf("healthy run must pass thresholds, code=%d", code)
	}
	// 制造不达标场景。
	bad := sum
	bad.RoundtripRate = 0.90
	if code := bad.Evaluate(DefaultThresholds()); code != 4 {
		t.Fatalf("metric failure must yield exit code 4, got %d", code)
	}

	dir := t.TempDir()
	m1 := &Manifest{Seed: 41, Source: "test", Summary: sum, Segments: segs}
	p1, err := m1.Write(dir)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := ReadManifest(filepath.Dir(p1))
	if err != nil {
		t.Fatal(err)
	}
	if len(m2.Segments) != len(segs) {
		t.Fatal("manifest roundtrip lost segments")
	}
	for i := range segs {
		if m2.Segments[i].SHA256 != segs[i].SHA256 {
			t.Fatal("manifest rerun hash mismatch")
		}
	}
}

func segID(i int) string {
	return "seg-" + string(rune('a'+i))
}

func hashStub(i int) string {
	b := make([]byte, 64)
	for j := range b {
		b[j] = '0'
	}
	b[63] = byte('0' + i%10)
	return string(b)
}
