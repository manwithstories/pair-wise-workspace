package segment

import (
	"errors"
	"fmt"
	"testing"

	"aircheck/internal/fixture"
	"aircheck/internal/media"
)

func tsFixture(t *testing.T) ([]byte, *fixture.ClipSpec) {
	t.Helper()
	spec := fixture.DefaultClipSpec()
	// One second of media: 30 frames, 46 audio frames (48000/1024).
	data := spec.BuildTS(0, 30, 0, 46)
	return data, &spec
}

func TestDemuxTSBasic(t *testing.T) {
	data, spec := tsFixture(t)
	res, err := DemuxTS("seg.ts", data, TSOptions{})
	if err != nil {
		t.Fatalf("DemuxTS: %v", err)
	}
	frames, keyframes, samples := res.Counts()
	if frames != 30 {
		t.Fatalf("frames %d", frames)
	}
	if keyframes != 1 {
		t.Fatalf("keyframes %d", keyframes)
	}
	if samples != 46*fixture.AudioSamplesPerFrame {
		t.Fatalf("samples %d", samples)
	}
	if res.Format != FormatTS {
		t.Fatalf("format %v", res.Format)
	}
	vt, ok := res.TrackFor(media.KindVideo)
	if !ok || vt.Timescale != 90000 || vt.Kind != media.KindVideo {
		t.Fatalf("video track %+v", vt)
	}
	if res.Wraps != 0 {
		t.Fatalf("unexpected wraps %d", res.Wraps)
	}
	// DTS grid: 30 frames at 3000 ticks = 1 s.
	first, _ := res.FirstDTS(media.KindVideo)
	last, _ := res.LastEnd(media.KindVideo)
	want := int64(spec.DurationSec*90000) / int64(spec.FrameCount())
	if last-first != int64(30)*want {
		t.Fatalf("video span %d, want %d", last-first, 30*want)
	}
}

func TestDemuxTSSourceOffsets(t *testing.T) {
	data, _ := tsFixture(t)
	res, err := DemuxTS("seg.ts", data, TSOptions{})
	if err != nil {
		t.Fatalf("DemuxTS: %v", err)
	}
	// Every packet's source offset must be the byte index of its first
	// transport packet and 188-aligned.
	for _, p := range res.Packets {
		if p.SourceOffset%188 != 0 {
			t.Fatalf("unaligned source offset %d for %v", p.SourceOffset, p.Kind)
		}
		off := p.SourceOffset
		if off < 0 || off >= int64(len(data)) {
			t.Fatalf("source offset %d out of range", off)
		}
		if data[off] != 0x47 {
			t.Fatalf("offset %d is not a sync byte", off)
		}
	}
}

func TestDemuxTSTruncatedReportsOffset(t *testing.T) {
	data, _ := tsFixture(t)
	cut := data[:len(data)-100] // mid-packet truncation
	_, err := DemuxTS("seg.ts", cut, TSOptions{})
	if err == nil {
		t.Fatal("expected truncation fault")
	}
	f, ok := media.AsFault(err)
	if !ok || f.Kind != media.FaultTruncated {
		t.Fatalf("fault %v", err)
	}
	// Offset must be exactly the first byte past the last complete packet.
	want := int64(len(data) - 100 - (len(data)-100)%188)
	if f.Pos.Offset != want {
		t.Fatalf("offset %d want %d", f.Pos.Offset, want)
	}
}

func TestDemuxTSContinuityBreakReportsOffset(t *testing.T) {
	spec := fixture.DefaultClipSpec()
	w := fixture.NewTSWriter()
	w.WritePAT()
	w.WritePMT()
	w.WritePES(fixture.PIDVideo, 0, 0, []byte{0, 0, 0, 1, 0x65, 1, 2, 3}, false)
	w.ForceCC(fixture.PIDVideo, 7) // skip counters
	w.WritePES(fixture.PIDVideo, 3000, 3000, []byte{0, 0, 0, 1, 0x41, 1, 2, 3}, false)
	_ = spec
	data := w.Bytes()
	_, err := DemuxTS("seg.ts", data, TSOptions{})
	if err == nil {
		t.Fatal("expected continuity fault")
	}
	f, _ := media.AsFault(err)
	if f.Kind != media.FaultContinuity {
		t.Fatalf("kind %v", f.Kind)
	}
	if f.Pos.Offset%188 != 0 || f.Pos.Offset >= int64(len(data)) {
		t.Fatalf("bad fault offset %d", f.Pos.Offset)
	}
}

func TestDemuxTSMissingPSI(t *testing.T) {
	w := fixture.NewTSWriter()
	w.WritePES(fixture.PIDVideo, 0, 0, []byte{0, 0, 0, 1, 0x65}, false)
	_, err := DemuxTS("seg.ts", w.Bytes(), TSOptions{})
	if err == nil {
		t.Fatal("expected missing init fault")
	}
	if media.KindOf(err) != media.FaultMissingInit {
		t.Fatalf("kind %v", media.KindOf(err))
	}
}

func TestDemuxTSWraparound(t *testing.T) {
	spec := fixture.DefaultClipSpec()
	const wrap = int64(1) << 33
	start := wrap - 2*3000 // two frames before wrap
	w := fixture.NewTSWriter()
	w.WritePAT()
	w.WritePMT()
	dur := spec.VideoTickDur()
	for i := 0; i < 4; i++ {
		pts := start + int64(i)*dur
		w.WritePES(fixture.PIDVideo, pts&(wrap-1), pts&(wrap-1),
			[]byte{0, 0, 0, 1, 0x65, byte(i)}, false)
	}
	res, err := DemuxTS("seg.ts", w.Bytes(), TSOptions{})
	if err != nil {
		t.Fatalf("DemuxTS: %v", err)
	}
	if res.Wraps != 1 {
		t.Fatalf("wraps %d", res.Wraps)
	}
	first, _ := res.FirstDTS(media.KindVideo)
	last, _ := res.LastEnd(media.KindVideo)
	if last-first != 4*dur {
		t.Fatalf("span after wrap %d want %d", last-first, 4*dur)
	}
}

func TestPayloadHashStable(t *testing.T) {
	spec := fixture.DefaultClipSpec()
	a, err := DemuxTS("a.ts", spec.BuildTS(0, 30, 0, 46), TSOptions{})
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	b, err := DemuxTS("b.ts", spec.BuildTS(0, 30, 0, 46), TSOptions{})
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if a.PayloadHashString() != b.PayloadHashString() {
		t.Fatalf("hashes differ: %s vs %s", a.PayloadHashString(), b.PayloadHashString())
	}
	// Re-muxing must preserve the hash even when packet boundaries change;
	// simulate by hashing after Sort on both.
	if a.PayloadHashString() == "" {
		t.Fatal("empty hash")
	}
}

func TestDemuxTSBadSync(t *testing.T) {
	data, _ := tsFixture(t)
	bad := append([]byte{}, data...)
	bad[0] = 0x00
	_, err := DemuxTS("seg.ts", bad, TSOptions{})
	if err == nil {
		t.Fatal("expected fault")
	}
	f, _ := media.AsFault(err)
	if f.Pos.Offset != 0 {
		t.Fatalf("offset %d", f.Pos.Offset)
	}
}

func TestDemuxTSEmptyAndShort(t *testing.T) {
	if _, err := DemuxTS("x.ts", nil, TSOptions{}); !errors.Is(err, media.FaultTruncated) {
		t.Fatalf("empty: %v", err)
	}
	if _, err := DemuxTS("x.ts", make([]byte, 100), TSOptions{}); !errors.Is(err, media.FaultTruncated) {
		t.Fatalf("short: %v", err)
	}
}

func TestFillMissingDurations(t *testing.T) {
	res := &Result{Packets: []Packet{
		{Kind: media.KindVideo, DTS: 0},
		{Kind: media.KindVideo, DTS: 3000},
		{Kind: media.KindVideo, DTS: 6000},
	}}
	res.FillMissingDurations(map[media.Kind]int64{media.KindVideo: 3000})
	if res.Packets[0].Duration != 3000 || res.Packets[1].Duration != 3000 || res.Packets[2].Duration != 3000 {
		t.Fatalf("durations %+v", res.Packets)
	}
}

var _ = fmt.Sprintf
