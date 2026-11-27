package pipeline

import (
	"testing"

	"tsqc/internal/clock"
	"tsqc/internal/ts"
)

func sampleAUs() []ts.AccessUnit {
	// Three IDRs, two P-frames between each, audio every 100 ms.
	var aus []ts.AccessUnit
	step := int64(clock.TicksPerSecond / 2)
	pts := int64(0)
	idr := true
	for i := 0; i < 9; i++ {
		data := []byte{0, 0, 0, 1, 0x01}
		if idr {
			data[4] = 0x05
		}
		aus = append(aus, ts.AccessUnit{
			PID: 0x101, Kind: ts.KindVideo, PTS: pts, DTS: pts, HasPTS: true, HasDTS: true,
			Keyframe: idr, Data: append(data, byte(i)),
		})
		aus = append(aus, ts.AccessUnit{
			PID: 0x102, Kind: ts.KindAudio, PTS: pts, HasPTS: true,
			Data: []byte{0xFF, 0xF1, 0x50, 0x80, 0x00, 0x1F, 0xFC, byte(i)},
		})
		pts += step / 3
		if (i+1)%3 == 0 {
			idr = true
		} else {
			idr = false
		}
	}
	return aus
}

func TestSegmentBuildAndVerify(t *testing.T) {
	aus := sampleAUs()
	seg := DefaultSegmenter(0x101, 0x102)
	segs, err := seg.Build(aus)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 3 {
		t.Fatalf("segments=%d", len(segs))
	}
	for i := range segs {
		if !segs[i].Keyframe || segs[i].Digest() == "" || segs[i].ContentSHA == "" {
			t.Fatalf("segment %d incomplete: %+v", i, segs[i])
		}
		if err := VerifySegment(&segs[i]); err != nil {
			t.Fatalf("verify segment %d: %v", i, err)
		}
		if segs[i].VideoAUs != 3 || segs[i].AudioAUs != 3 {
			t.Fatalf("segment %d au counts v=%d a=%d", i, segs[i].VideoAUs, segs[i].AudioAUs)
		}
	}
}

func TestSegmentDeterministic(t *testing.T) {
	aus := sampleAUs()
	seg := DefaultSegmenter(0x101, 0x102)
	a, err := seg.Build(aus)
	if err != nil {
		t.Fatal(err)
	}
	b, err := seg.Build(aus)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatal("segment count differs")
	}
	for i := range a {
		if a[i].Digest() != b[i].Digest() || a[i].ContentSHA != b[i].ContentSHA {
			t.Fatalf("segment %d not deterministic", i)
		}
	}
}

func TestIndexKeyframeHitRate(t *testing.T) {
	aus := sampleAUs()
	pmt := &ts.PMT{Program: 1, PCRPID: 0x101, Streams: []ts.Stream{
		{Type: ts.StreamH264, PID: 0x101}, {Type: ts.StreamAAC, PID: 0x102},
	}}
	ix := BuildIndex(aus, pmt)
	if len(ix.Keyframes) != 3 || ix.VideoAUs != 9 || ix.AudioAUs != 9 {
		t.Fatalf("index %+v", ix)
	}
	segs, err := DefaultSegmenter(0x101, 0x102).Build(aus)
	if err != nil {
		t.Fatal(err)
	}
	if got := ix.KeyframeHitRate(segs); got != 100 {
		t.Fatalf("hit rate=%v", got)
	}
	if kf, ok := ix.KeyframeAtOrAfter(clock.TicksPerSecond / 2); !ok || kf.Segment != 1 {
		t.Fatalf("lookup %+v ok=%v", kf, ok)
	}
}

func TestTimestampCheckRegressionsAndSkew(t *testing.T) {
	chk := &TimestampCheck{}
	feed := func(kind ts.Kind, pts int64) error {
		return chk.Feed(ts.AccessUnit{Kind: kind, PTS: pts, HasPTS: true})
	}
	if err := feed(ts.KindVideo, 1000); err != nil {
		t.Fatal(err)
	}
	if err := feed(ts.KindAudio, 1000); err != nil {
		t.Fatal(err)
	}
	if err := feed(ts.KindVideo, 500); !clock.IsCode(err, clock.CodeTimestampRegression) {
		t.Fatalf("want regression, got %v", err)
	}
	chk2 := &TimestampCheck{}
	chk2.Feed(ts.AccessUnit{Kind: ts.KindVideo, PTS: 0, HasPTS: true})
	chk2.Feed(ts.AccessUnit{Kind: ts.KindAudio, PTS: int64(clock.TicksPerMilli * 80), HasPTS: true})
	chk2.Feed(ts.AccessUnit{Kind: ts.KindVideo, PTS: 1, HasPTS: true})
	if chk2.SkewErrors == 0 || chk2.SyncError == nil {
		t.Fatalf("skew not flagged: %+v", chk2)
	}
}

func TestGapBackSegmentFlag(t *testing.T) {
	aus := []ts.AccessUnit{
		{PID: 0x101, Kind: ts.KindVideo, PTS: 0, HasPTS: true, Keyframe: true, Data: []byte{0, 0, 1, 0x05}},
		{PID: 0x101, Kind: ts.KindVideo, PTS: 100, HasPTS: true, Unrecoverable: true, Reason: "gap", Data: []byte{0, 0, 1, 0x01}},
		{PID: 0x101, Kind: ts.KindVideo, PTS: 200, HasPTS: true, Keyframe: true, Data: []byte{0, 0, 1, 0x05}},
	}
	segs, err := DefaultSegmenter(0x101, 0x102).Build(aus)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 {
		t.Fatalf("segments=%d", len(segs))
	}
	if segs[0].GapBack || !segs[1].GapBack {
		t.Fatalf("gap flags %v %v", segs[0].GapBack, segs[1].GapBack)
	}
	if segs[0].VideoAUs != 1 || segs[1].VideoAUs != 1 {
		t.Fatalf("damaged AU leaked into output")
	}
}
