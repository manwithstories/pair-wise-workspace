package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"relaycheck/internal/model"
)

func TestParseHLSExactTimeline(t *testing.T) {
	src := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-MEDIA-SEQUENCE:0\n" +
		"#EXTINF:1.006,\nseg0.m4s\n#EXTINF:1.006,\nseg1.m4s\n" +
		"#EXTINF:0.988,\nseg2.m4s\n#EXT-X-ENDLIST\n"
	pl, err := ParseHLS(strings.NewReader(src))
	if err != nil {
		t.Fatalf("ParseHLS: %v", err)
	}
	if len(pl.Refs) != 3 || pl.Seq0 != 0 {
		t.Fatalf("refs=%d seq0=%d", len(pl.Refs), pl.Seq0)
	}
	// 1.006 秒必须精确为 1_006_000_000 ns，而非浮点近似。
	if got := pl.Refs[0].Duration.RoundNanos(); got != 1_006_000_000 {
		t.Fatalf("duration ns = %d", got)
	}
	// 两个 1.006 + 0.988 = 3.000s，逐项有理数累加无漂移。
	if got := pl.Refs[2].Start.RoundNanos(); got != 2_012_000_000 {
		t.Fatalf("seg2 start = %d, want 2012000000", got)
	}
}

func TestParseHLSMediaSequenceOffset(t *testing.T) {
	src := "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:10\n#EXTINF:2.0,\na.m4s\n#EXTINF:2.0,\nb.m4s\n"
	pl, err := ParseHLS(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if pl.Refs[0].Seq != 10 || pl.Refs[1].Seq != 11 {
		t.Fatalf("seqs = %d,%d", pl.Refs[0].Seq, pl.Refs[1].Seq)
	}
}

func TestParseDASHTimelineRepeats(t *testing.T) {
	mpd := `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period><AdaptationSet>
<SegmentTemplate timescale="90000" media="seg$Number$.m4s" startNumber="1">
<SegmentTimeline><S t="0" d="90000" r="2"/><S d="45000"/></SegmentTimeline>
</SegmentTemplate></AdaptationSet></Period></MPD>`
	pl, err := ParseDASH(strings.NewReader(mpd))
	if err != nil {
		t.Fatalf("ParseDASH: %v", err)
	}
	if len(pl.Refs) != 4 || pl.Seq0 != 1 {
		t.Fatalf("refs=%d seq0=%d", len(pl.Refs), pl.Seq0)
	}
	if pl.Refs[0].URI != "seg1.m4s" || pl.Refs[3].URI != "seg4.m4s" {
		t.Fatalf("uris = %s,%s", pl.Refs[0].URI, pl.Refs[3].URI)
	}
	if got := pl.Refs[0].Duration.RoundNanos(); got != 1_000_000_000 {
		t.Fatalf("d0 = %d", got)
	}
	if got := pl.Refs[3].Duration.RoundNanos(); got != 500_000_000 {
		t.Fatalf("d3 = %d", got)
	}
	// seg4 起点 = 3 * 1s。
	if got := pl.Refs[3].Start.RoundNanos(); got != 3_000_000_000 {
		t.Fatalf("start3 = %d", got)
	}
}

func TestParseDASHRejectsZeroTimescale(t *testing.T) {
	mpd := `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period><AdaptationSet>
<SegmentTemplate timescale="0" media="seg$Number$.m4s" startNumber="1">
<SegmentTimeline><S t="0" d="90000"/></SegmentTimeline>
</SegmentTemplate></AdaptationSet></Period></MPD>`
	if _, err := ParseDASH(strings.NewReader(mpd)); err == nil {
		t.Fatal("expected error for zero timescale")
	}
}

func TestCrossCheckGapAndMissingFileAndMismatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "seg0.m4s"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// seg2 物理文件缺失；seg1 在 HLS 与 DASH 间引用名不一致。
	hls := Playlist{Kind: "hls", Refs: []model.SegmentRef{
		{Seq: 0, URI: "seg0.m4s", Start: model.Nano(0), Duration: model.Nano(1_000_000_000)},
		{Seq: 1, URI: "hls1.m4s", Start: model.Nano(1_000_000_000), Duration: model.Nano(1_000_000_000)},
		{Seq: 3, URI: "seg3.m4s", Start: model.Nano(3_000_000_000), Duration: model.Nano(1_000_000_000)},
	}}
	dash := Playlist{Kind: "dash", Refs: []model.SegmentRef{
		{Seq: 0, URI: "seg0.m4s", Start: model.Nano(0), Duration: model.Nano(1_000_000_000)},
		{Seq: 1, URI: "dash1.m4s", Start: model.Nano(1_000_000_000), Duration: model.Nano(1_000_000_000)},
		{Seq: 3, URI: "seg3.m4s", Start: model.Nano(3_000_000_000), Duration: model.Nano(1_000_000_000)},
	}}
	rep := CrossCheck(dir, hls, dash)
	kinds := map[string]int{}
	for _, f := range rep.Faults {
		kinds[f.Kind]++
	}
	if kinds[model.FaultManifestMissing] < 2 { // seq2 序号缺口 + 缺失物理文件
		t.Fatalf("missing faults = %d (%+v)", kinds[model.FaultManifestMissing], rep.Faults)
	}
	if kinds[model.FaultManifestMismatch] != 1 {
		t.Fatalf("mismatch faults = %d", kinds[model.FaultManifestMismatch])
	}
	if len(rep.Segments) != 3 || rep.Segments[0].Exists != true {
		t.Fatalf("segments = %+v", rep.Segments)
	}
}

func TestCrossCheckDuplicate(t *testing.T) {
	hls := Playlist{Refs: []model.SegmentRef{
		{Seq: 0, URI: "a.m4s"}, {Seq: 0, URI: "a.m4s"},
	}}
	dash := Playlist{Refs: []model.SegmentRef{{Seq: 0, URI: "a.m4s"}}}
	rep := CrossCheck(t.TempDir(), hls, dash)
	found := false
	for _, f := range rep.Faults {
		if f.Kind == model.FaultManifestDuplicate && f.Segment == 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("duplicate not detected: %+v", rep.Faults)
	}
}
