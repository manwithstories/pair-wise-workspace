package timeline

import (
	"testing"

	"bq/internal/clock"
	"bq/internal/manifest"
	"bq/internal/model"
)

func buildTestManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	return &manifest.Manifest{
		Version: "bq/1", Seed: 1, DurationS: 10, MasterHz: 90000,
		Streams: []manifest.Stream{
			{ID: 0, Kind: manifest.KindVideo, Codec: "h264", Container: "mp4", File: "v.mp4",
				Width: 16, Height: 16, TimebaseNum: 90000, TimebaseDen: 1, StartPTS: 0,
				WrapPeriod: 1000, KeyframeEvery: 2,
				EditList:   []manifest.EditRange{{StartPTS: 0, EndPTS: 90000}}},
			{ID: 1, Kind: manifest.KindAudio, Codec: "aac", Container: "mp4", File: "a.mp4",
				TimebaseNum: 48000, TimebaseDen: 1, StartPTS: 0, KeyframeEvery: 1},
		},
	}
}

func TestComposeNormalizesTimebases(t *testing.T) {
	m := buildTestManifest(t)
	pkts := map[int][]manifest.Packet{
		0: {{StreamID: 0, Seq: 0, Kind: manifest.KindMedia, GroupID: 0, SourceOffset: 0,
			RawPTS: 0, DurationTick: 90000, Keyframe: true},
			{StreamID: 0, Seq: 1, Kind: manifest.KindMedia, GroupID: 0, SourceOffset: 10,
				RawPTS: 90000, DurationTick: 90000}},
		1: {{StreamID: 1, Seq: 0, Kind: manifest.KindMedia, GroupID: 1, SourceOffset: 0,
			RawPTS: 0, DurationTick: 48000},
			{StreamID: 1, Seq: 1, Kind: manifest.KindMedia, GroupID: 1, SourceOffset: 5,
			RawPTS: 48000, DurationTick: 48000}},
	}
	tl, errs := Compose(m, pkts, map[int]*clock.WrapExpander{}, map[int][]clock.WrapEvent{})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if tl.Streams[0][1].MasterPTS != 90000 {
		t.Fatalf("video master pts wrong: %d", tl.Streams[0][1].MasterPTS)
	}
	// 48k source tick at one second maps to 90000 master ticks.
	if tl.Streams[1][1].MasterPTS != 90000 {
		t.Fatalf("audio master pts wrong: %d", tl.Streams[1][1].MasterPTS)
	}
	if len(tl.Edits[0]) != 1 || tl.Edits[0][0].EndMaster != 90000 {
		t.Fatalf("edit conversion wrong: %+v", tl.Edits[0])
	}
	// Provenance retained.
	if tl.Streams[0][1].RawPTS != 90000 || tl.Streams[0][1].SourceOffset != 10 {
		t.Fatal("source provenance lost")
	}
}

func TestComposeAppliesWrapEvents(t *testing.T) {
	m := buildTestManifest(t)
	pkts := map[int][]manifest.Packet{
		0: {{StreamID: 0, Seq: 0, Kind: manifest.KindMedia, GroupID: 0, SourceOffset: 0, RawPTS: 950, Keyframe: true},
			{StreamID: 0, Seq: 1, Kind: manifest.KindMedia, GroupID: 0, SourceOffset: 10, RawPTS: 50}},
	}
	events := map[int][]clock.WrapEvent{
		0: {{StreamID: 0, WrapCount: 1, RawPTS: 50, PrevRawPTS: 950,
			UnwrappedPTS: 1050, SourceOffset: 10}},
	}
	tl, errs := Compose(m, pkts, map[int]*clock.WrapExpander{}, events)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	item := tl.Streams[0][1]
	if item.UnwrappedPTS != 1050 || item.WrapCount != 1 {
		t.Fatalf("wrap not applied: %+v", item)
	}
	// Monotonic on master axis.
	if tl.Streams[0][1].MasterPTS <= tl.Streams[0][0].MasterPTS {
		t.Fatal("master timeline not monotonic after wrap")
	}
}

func TestKeyframeLocation(t *testing.T) {
	items := []Item{
		{MasterPTS: 0, Keyframe: true},
		{MasterPTS: 90000, Keyframe: false},
		{MasterPTS: 180000, Keyframe: true},
		{MasterPTS: 270000, Keyframe: false},
	}
	kf, ok := NextKeyframeAtOrAfter(items, 200000)
	if !ok || kf.MasterPTS != 180000 {
		t.Fatalf("keyframe seek wrong: ok=%v pts=%d", ok, kf.MasterPTS)
	}
	if _, ok := NextKeyframeAtOrAfter(items, -1); ok {
		t.Fatal("no keyframe should be found before the first tick")
	}
	prev, ok := ItemsAtOrBefore(items, 200000)
	if !ok || prev.MasterPTS != 180000 {
		t.Fatalf("at-or-before wrong: %d", prev.MasterPTS)
	}
}

func TestEstimateDriftRecoverable(t *testing.T) {
	m := buildTestManifest(t)
	m.Anchors = []manifest.Anchor{
		{StreamID: 0, AtSecond: 0, SourcePTS: 0, MasterTicks: 900}, // 10ms fixed offset
		{StreamID: 0, AtSecond: 5, SourcePTS: 5 * 90000, MasterTicks: 5*90000 + 900},
	}
	tl, _ := Compose(m, map[int][]manifest.Packet{},
		map[int]*clock.WrapExpander{}, map[int][]clock.WrapEvent{})
	reports, errs := EstimateDrift(m, tl)
	if len(errs) != 0 || len(reports[0].Unrecoverable) != 0 {
		t.Fatalf("clean anchors must recover, errs=%v", errs)
	}
	seg := reports[0].Segments[0]
	if seg.OffsetMaster != 900 || seg.SlopePpm != 0 || !seg.Recoverable {
		t.Fatalf("segment fit wrong: %+v", seg)
	}
	off, ok := reports[0].SyncOffsetAt(2 * clock.MasterHz)
	if !ok || off != 900 {
		t.Fatalf("sync offset wrong: off=%d ok=%v", off, ok)
	}
}

func TestEstimateDriftAnchorConflict(t *testing.T) {
	m := buildTestManifest(t)
	m.Anchors = []manifest.Anchor{
		{StreamID: 0, AtSecond: 0, SourcePTS: 0, MasterTicks: 0},
		{StreamID: 0, AtSecond: 5, SourcePTS: 5 * 90000, MasterTicks: 5*90000 + 50*90, Conflicts: true},
	}
	tl, _ := Compose(m, map[int][]manifest.Packet{},
		map[int]*clock.WrapExpander{}, map[int][]clock.WrapEvent{})
	reports, errs := EstimateDrift(m, tl)
	if len(errs) != 1 || errs[0].Code != model.ErrAnchorConflict {
		t.Fatalf("want anchor conflict error, got %v", errs)
	}
	if len(reports[0].Unrecoverable) != 1 {
		t.Fatal("conflict must open unrecoverable range")
	}
	if reports[0].Segments[0].Recoverable {
		t.Fatal("conflicted segment must be marked unrecoverable")
	}
	// Source offset must never be rewritten: error keeps raw anchor PTS.
	if errs[0].OriginalPTS != 0 || errs[0].HasPTS != true {
		t.Fatalf("provenance missing: %+v", errs[0])
	}
	if _, ok := reports[0].SyncOffsetAt(2 * clock.MasterHz); ok {
		t.Fatal("unrecoverable segment must report false")
	}
}
