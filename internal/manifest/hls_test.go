package manifest

import (
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"

	"mediad/internal/model"
)

func TestParseHLS_BasicTimeline(t *testing.T) {
	fsys := fstest.MapFS{
		"pl.m3u8": {Data: []byte("#EXTM3U\n#EXT-X-VERSION:4\n#EXT-X-TARGETDURATION:6\n" +
			"#EXTINF:6.0,\na0.m4s\n#EXTINF:6.0,\na1.m4s\n#EXT-X-ENDLIST\n")},
		"a0.m4s": {Data: make([]byte, 100)},
		"a1.m4s": {Data: make([]byte, 200)},
	}
	tl, err := ParseHLS(fsys, "pl.m3u8", model.TrackStudio)
	if err != nil {
		t.Fatalf("ParseHLS: %v", err)
	}
	if tl.Partial {
		t.Fatalf("complete playlist must not be partial: %+v", tl.Missing)
	}
	segs := tl.Tracks[model.TrackStudio]
	if len(segs) != 2 {
		t.Fatalf("segments = %d, want 2", len(segs))
	}
	if segs[0].StartMS != 0 || segs[0].DurationMS != 6000 {
		t.Fatalf("seg0 span = %d+%d ms", segs[0].StartMS, segs[0].DurationMS)
	}
	if segs[1].StartMS != 6000 {
		t.Fatalf("seg1 start = %d ms, want 6000", segs[1].StartMS)
	}
	if segs[1].ByteSize != 200 {
		t.Fatalf("seg1 size = %d, want 200", segs[1].ByteSize)
	}
}

func TestParseHLS_DecimalIsExact(t *testing.T) {
	fsys := fstest.MapFS{
		"pl.m3u8": {Data: []byte("#EXTM3U\n#EXTINF:0.1,\na0.m4s\n" +
			"#EXTINF:0.2,\na1.m4s\n#EXT-X-ENDLIST\n")},
		"a0.m4s": {Data: []byte("x")},
		"a1.m4s": {Data: []byte("yy")},
	}
	tl, err := ParseHLS(fsys, "pl.m3u8", model.TrackField)
	if err != nil {
		t.Fatalf("ParseHLS: %v", err)
	}
	segs := tl.Tracks[model.TrackField]
	if got := segs[1].StartMS; got != 100 {
		t.Fatalf("seg1 start = %d ms, want 100", got)
	}
	end, err := segs[1].End()
	if err != nil {
		t.Fatalf("End: %v", err)
	}
	endMS, err := end.Millis()
	if err != nil {
		t.Fatalf("Millis: %v", err)
	}
	if endMS != 300 {
		t.Fatalf("0.1+0.2 cumulative end = %d ms, want exact 300", endMS)
	}
}

func TestParseHLS_ByteRangeChaining(t *testing.T) {
	fsys := fstest.MapFS{
		"pl.m3u8": {Data: []byte("#EXTM3U\n#EXTINF:2.0,\n#EXT-X-BYTERANGE:10@0\nblob.mp4\n" +
			"#EXTINF:2.0,\n#EXT-X-BYTERANGE:15\nblob.mp4\n#EXT-X-ENDLIST\n")},
		"blob.mp4": {Data: make([]byte, 40)},
	}
	tl, err := ParseHLS(fsys, "pl.m3u8", model.TrackVoice)
	if err != nil {
		t.Fatalf("ParseHLS: %v", err)
	}
	segs := tl.Tracks[model.TrackVoice]
	if segs[0].ByteStart != 0 || segs[0].ByteSize != 10 {
		t.Fatalf("seg0 range = %d+%d", segs[0].ByteStart, segs[0].ByteSize)
	}
	if segs[1].ByteStart != 10 || segs[1].ByteSize != 15 {
		t.Fatalf("seg1 range = %d+%d, want chained 10+15", segs[1].ByteStart, segs[1].ByteSize)
	}
}

func TestParseHLS_MissingSegmentIsPartial(t *testing.T) {
	fsys := fstest.MapFS{
		"pl.m3u8": {Data: []byte("#EXTM3U\n#EXTINF:6.0,\na0.m4s\n#EXTINF:6.0,\na1.m4s\n#EXT-X-ENDLIST\n")},
		"a0.m4s":  {Data: []byte("ok")},
	}
	tl, err := ParseHLS(fsys, "pl.m3u8", model.TrackStudio)
	if err != nil {
		t.Fatalf("partial index must not hard-fail: %v", err)
	}
	if !tl.Partial || len(tl.Missing) != 1 {
		t.Fatalf("want 1 missing marker, got %+v", tl.Missing)
	}
	m := tl.Missing[0]
	if m.Index != 1 || m.URI != "a1.m4s" || m.Track != model.TrackStudio {
		t.Fatalf("missing marker = %+v", m)
	}
	if len(tl.Tracks[model.TrackStudio]) != 1 {
		t.Fatalf("confirmed segment must remain, got %d", len(tl.Tracks[model.TrackStudio]))
	}
}

func TestParseHLS_NoEndlistIsPartial(t *testing.T) {
	fsys := fstest.MapFS{
		"pl.m3u8": {Data: []byte("#EXTM3U\n#EXTINF:6.0,\na0.m4s\n")},
		"a0.m4s":  {Data: []byte("ok")},
	}
	tl, err := ParseHLS(fsys, "pl.m3u8", model.TrackField)
	if err != nil {
		t.Fatalf("ParseHLS: %v", err)
	}
	if !tl.Partial {
		t.Fatal("playlist without ENDLIST must be partial")
	}
	if tl.Missing[0].Reason == "" {
		t.Fatal("missing reason must be populated")
	}
}

func TestParseHLS_NegativeAndBadInput(t *testing.T) {
	cases := map[string]string{
		"no header":     "#EXTINF:6.0,\na0.m4s\n#EXT-X-ENDLIST\n",
		"zero duration": "#EXTM3U\n#EXTINF:0.0,\na0.m4s\n#EXT-X-ENDLIST\n",
		"bad decimal":   "#EXTM3U\n#EXTINF:x,\na0.m4s\n#EXT-X-ENDLIST\n",
		"missing file":  "nope.m3u8",
	}
	for name, pl := range cases {
		t.Run(name, func(t *testing.T) {
			fsys := fstest.MapFS{}
			if name != "missing file" {
				fsys["pl.m3u8"] = &fstest.MapFile{Data: []byte(pl)}
				fsys["a0.m4s"] = &fstest.MapFile{Data: []byte("x")}
			}
			_, err := ParseHLS(fsys, "pl.m3u8", model.TrackStudio)
			if err == nil {
				t.Fatal("expected error")
			}
			var pe *fs.PathError
			if name == "missing file" && !errors.As(err, &pe) {
				t.Fatalf("missing playlist must surface PathError, got %T %v", err, err)
			}
		})
	}
}
