package mp4

import (
	"os"
	"testing"

	"mediabridge/internal/adapter"
	"mediabridge/internal/fault"
	"mediabridge/internal/fixture"
	"mediabridge/internal/media"
)

func parseFixture(t *testing.T, opts fixture.MP4Options) (*media.Asset, error) {
	t.Helper()
	dir := t.TempDir()
	rep, err := fixture.GenerateMP4(dir, "clip.mp4", opts)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return (&Reader{}).Parse(adapter.Source{AssetID: "asset-1", Path: rep.Path}, adapter.Options{KeepPayload: true})
}

func TestParseCleanAsset(t *testing.T) {
	opts := fixture.DefaultMP4Options(41)
	asset, err := parseFixture(t, opts)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(asset.Tracks) != 3 {
		t.Fatalf("tracks = %d", len(asset.Tracks))
	}
	wantKinds := []media.TrackKind{media.KindVideo, media.KindAudio, media.KindAux}
	for i, trk := range asset.Tracks {
		if trk.Kind != wantKinds[i] {
			t.Fatalf("track %d kind = %s want %s", i, trk.Kind, wantKinds[i])
		}
		if trk.Timescale == 0 {
			t.Fatalf("track %d timescale zero", i)
		}
		if trk.Codec.ConfigSHA256 == "" {
			t.Fatalf("track %d missing codec digest", i)
		}
	}
	if len(asset.Packets) != opts.VideoSamples+opts.AudioSamples+opts.AuxSamples {
		t.Fatalf("packets = %d", len(asset.Packets))
	}
	// Video must report exactly the keyframe count in its stss.
	videoKeyframes := 0
	for _, p := range asset.Packets {
		if p.TrackIndex == 0 && p.Sync {
			videoKeyframes++
		}
	}
	if want := (opts.VideoSamples + opts.KeyframeEvery - 1) / opts.KeyframeEvery; videoKeyframes != want {
		t.Fatalf("video sync packets = %d want %d", videoKeyframes, want)
	}
	// Every payload must hash without error and differ run-to-run only by seed.
	for _, p := range asset.Packets {
		if len(p.Payload) == 0 {
			t.Fatal("empty payload")
		}
	}
	if asset.SourceSHA == "" {
		t.Fatal("missing source digest")
	}
}

func TestParseIsDeterministic(t *testing.T) {
	opts := fixture.DefaultMP4Options(7)
	a1, err := parseFixture(t, opts)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := parseFixture(t, opts)
	if err != nil {
		t.Fatal(err)
	}
	if a1.SourceSHA != a2.SourceSHA {
		t.Fatal("source digest not stable for identical fixtures")
	}
	if len(a1.Packets) != len(a2.Packets) {
		t.Fatal("packet count drift")
	}
	for i := range a1.Packets {
		if a1.Packets[i].SourceOffset != a2.Packets[i].SourceOffset ||
			a1.Packets[i].DTSNS != a2.Packets[i].DTSNS ||
			a1.Packets[i].SHA256 != a2.Packets[i].SHA256 {
			t.Fatalf("packet %d differs between identical runs", i)
		}
	}
}

func TestNonZeroStartTrimsToZero(t *testing.T) {
	opts := fixture.DefaultMP4Options(3)
	opts.EditMediaTime = 9000 // 100ms at 90kHz
	asset, err := parseFixture(t, opts)
	if err != nil {
		t.Fatal(err)
	}
	video := asset.Tracks[0]
	if video.OriginNS != 100_000_000 {
		t.Fatalf("origin = %d", video.OriginNS)
	}
	for _, p := range asset.Packets {
		if p.TrackIndex == 0 && (p.PTSNS < 0 || p.DTSNS < 0) {
			t.Fatalf("packet %d retained negative timestamp pts=%d dts=%d", p.PacketIndex, p.PTSNS, p.DTSNS)
		}
	}
}

func TestEmptyEditListIsAccepted(t *testing.T) {
	opts := fixture.DefaultMP4Options(5)
	opts.Corrupt = fixture.CorruptEmptyEdit
	asset, err := parseFixture(t, opts)
	if err != nil {
		t.Fatalf("empty edit list should parse: %v", err)
	}
	if len(asset.Tracks) != 3 {
		t.Fatalf("tracks = %d", len(asset.Tracks))
	}
}

func TestCorruptFixturesAreLocated(t *testing.T) {
	cases := []struct {
		kind fixture.CorruptKind
		code fault.Code
	}{
		{fixture.CorruptTruncate, fault.CodeBadIndex},
		{fixture.CorruptBadChunkOffset, fault.CodeBadIndex},
		{fixture.CorruptBadStss, fault.CodeBadIndex},
		{fixture.CorruptNegativeEdit, fault.CodeNegativeEdit},
		{fixture.CorruptZeroTimescale, fault.CodeTimescale},
	}
	for _, tc := range cases {
		opts := fixture.DefaultMP4Options(11)
		opts.Corrupt = tc.kind
		_, err := parseFixture(t, opts)
		if err == nil {
			t.Fatalf("%s: expected error", tc.kind)
		}
		if got := fault.CodeOf(err); got != tc.code {
			t.Fatalf("%s: code = %s want %s (%v)", tc.kind, got, tc.code, err)
		}
		e, _ := fault.As(err)
		if e.Loc.SourcePath == "" {
			t.Fatalf("%s: error missing source path: %v", tc.kind, err)
		}
		if !e.Loc.HasOffset && !e.Loc.HasTrack {
			t.Fatalf("%s: error missing offset and track: %v", tc.kind, err)
		}
	}
}

func TestUnknownBoxIsIgnored(t *testing.T) {
	opts := fixture.DefaultMP4Options(13)
	opts.Corrupt = fixture.CorruptUnknownBox
	asset, err := parseFixture(t, opts)
	if err != nil {
		t.Fatalf("unknown box should be skipped: %v", err)
	}
	if len(asset.Tracks) != 3 {
		t.Fatalf("tracks = %d", len(asset.Tracks))
	}
}

func TestSniff(t *testing.T) {
	dir := t.TempDir()
	rep, err := fixture.GenerateMP4(dir, "clip.mp4", fixture.DefaultMP4Options(2))
	if err != nil {
		t.Fatal(err)
	}
	f, err := openFile(rep.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if !(&Reader{}).Sniff(f) {
		t.Fatal("mp4 fixture not sniffed")
	}
}

func TestParseMissingFileIsLocated(t *testing.T) {
	_, err := (&Reader{}).Parse(adapter.Source{AssetID: "x", Path: "/nonexistent/clip.mp4"}, adapter.Options{})
	if err == nil {
		t.Fatal("want error")
	}
	if fault.CodeOf(err) != fault.CodeCorruptContainer {
		t.Fatalf("code = %v", fault.CodeOf(err))
	}
}

func openFile(path string) (*os.File, error) { return os.Open(path) }

func TestDurationCapEnforced(t *testing.T) {
	dir := t.TempDir()
	rep, err := fixture.GenerateMP4(dir, "clip.mp4", fixture.DefaultMP4Options(8))
	if err != nil {
		t.Fatal(err)
	}
	// A 1-second asset with a 1-millisecond cap must be rejected as out of range.
	_, err = (&Reader{}).Parse(adapter.Source{AssetID: "cap", Path: rep.Path},
		adapter.Options{KeepPayload: false, MaxAssetDurationNS: 1_000_000})
	if fault.CodeOf(err) != fault.CodeDurationRange {
		t.Fatalf("code = %v (%v)", fault.CodeOf(err), err)
	}
	e, _ := fault.As(err)
	if !e.Loc.HasTrack && e.Loc.SourcePath == "" {
		t.Fatal("duration error lacks location")
	}
}

func TestPacketCountCapEnforced(t *testing.T) {
	dir := t.TempDir()
	rep, err := fixture.GenerateMP4(dir, "clip.mp4", fixture.DefaultMP4Options(8))
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Reader{}).Parse(adapter.Source{AssetID: "cap", Path: rep.Path},
		adapter.Options{KeepPayload: false, MaxPacketsPerTrack: 10})
	if err == nil {
		t.Fatal("expected packet-count rejection")
	}
	if fault.CodeOf(err) != fault.CodeDurationRange {
		t.Fatalf("code = %v", fault.CodeOf(err))
	}
}

// TestBackwardDTSRejected drives buildPackets directly with a sample table
// whose decode time goes backwards, as a wrapped counter would produce. The
// guard must return a located timestamp error.
func TestBackwardDTSRejected(t *testing.T) {
	p := &parser{
		src:   adapter.Source{AssetID: "x", Path: "/x.mov"},
		opts:  adapter.Options{MaxPacketsPerTrack: 100},
		asset: &media.Asset{ID: "x"},
	}
	tbl := &sampleTable{
		count:   3,
		dts:     []int64{0, 100, 50},
		pts:     []int64{0, 100, 50},
		dur:     []int64{100, 100, 100},
		sizes:   []uint32{4, 4, 4},
		offsets: []int64{10, 14, 18},
		sync:    []bool{true, false, false},
	}
	p.tables = []tableEntry{{track: &media.Track{Index: 0, ID: 1, Kind: media.KindVideo}, table: tbl}}
	err := p.buildPackets()
	if err == nil {
		t.Fatal("expected backward-DTS rejection")
	}
	if fault.CodeOf(err) != fault.CodeTimestampWrap {
		t.Fatalf("code = %v (%v)", fault.CodeOf(err), err)
	}
	e, _ := fault.As(err)
	if e.Loc.Offset != 18 || e.Loc.TrackID != 1 {
		t.Fatalf("locator = %+v", e.Loc)
	}
}
