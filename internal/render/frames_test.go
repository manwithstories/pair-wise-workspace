package render

import (
	"bytes"
	"context"
	"encoding/binary"
	"image/png"
	"testing"

	"archivetl/internal/clock"
	"archivetl/internal/pipeline"
	"archivetl/internal/subtitle"
)

func testSegment() *pipeline.Segment {
	seg := &pipeline.Segment{
		ID: 3, Start: clock.MillisToTick(9000), End: clock.MillisToTick(12000),
	}
	for i, text := range []string{"alpha", "beta", "gamma"} {
		seg.Records = append(seg.Records, subtitle.Record{
			Track: "trk", Source: "in.srt", Sequence: i + 1,
			Offset: int64(i * 40),
			Start:  clock.MillisToTick(int64(9000 + i*700)),
			End:    clock.MillisToTick(int64(9000 + i*700 + 500)),
			Text:   text,
		})
	}
	return seg
}

// decodeContainer parses the AFR1 header and decodes every embedded PNG.
func decodeContainer(t *testing.T, raw []byte) int {
	t.Helper()
	if string(raw[:4]) != Magic {
		t.Fatalf("bad magic: %q", raw[:4])
	}
	w := binary.BigEndian.Uint32(raw[4:8])
	h := binary.BigEndian.Uint32(raw[8:12])
	n := binary.BigEndian.Uint32(raw[12:16])
	if w != FrameW || h != FrameH || n == 0 {
		t.Fatalf("header wrong: %dx%d frames=%d", w, h, n)
	}
	pos := 16
	for i := uint32(0); i < n; i++ {
		if pos+4 > len(raw) {
			t.Fatal("truncated frame length")
		}
		flen := int(binary.BigEndian.Uint32(raw[pos : pos+4]))
		pos += 4
		if pos+flen > len(raw) {
			t.Fatalf("frame %d exceeds container", i)
		}
		img, err := png.Decode(bytes.NewReader(raw[pos : pos+flen]))
		if err != nil {
			t.Fatalf("frame %d not a decodable PNG: %v", i, err)
		}
		b := img.Bounds()
		if b.Dx() != FrameW || b.Dy() != FrameH {
			t.Fatalf("frame %d size %dx%d", i, b.Dx(), b.Dy())
		}
		pos += flen
	}
	if pos != len(raw) {
		t.Fatalf("trailing bytes: consumed %d of %d", pos, len(raw))
	}
	return int(n)
}

func TestRenderDeterministic(t *testing.T) {
	r := FrameRenderer{}
	first, h1, err := r.Render(context.Background(), testSegment())
	if err != nil {
		t.Fatal(err)
	}
	second, h2, err := r.Render(context.Background(), testSegment())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("identical segments produced different bytes")
	}
	if h1 != h2 || len(h1) != 64 {
		t.Fatalf("hash unstable or wrong length: %q %q", h1, h2)
	}
}

func TestRenderContainer(t *testing.T) {
	r := FrameRenderer{FramesPerSegment: 5}
	raw, hash, err := r.Render(context.Background(), testSegment())
	if err != nil {
		t.Fatal(err)
	}
	if n := decodeContainer(t, raw); n != 5 {
		t.Fatalf("frames=%d want 5", n)
	}
	if len(hash) != 64 {
		t.Fatalf("hash length=%d", len(hash))
	}
}

func TestRenderEmptySegment(t *testing.T) {
	seg := &pipeline.Segment{ID: 0, Start: 0, End: slotTicks}
	r := FrameRenderer{}
	raw, _, err := r.Render(context.Background(), seg)
	if err != nil {
		t.Fatal(err)
	}
	decodeContainer(t, raw)
}

func TestInputFingerprintStableAndSensitive(t *testing.T) {
	a := testSegment()
	b := testSegment()
	if InputFingerprint(a) != InputFingerprint(b) {
		t.Fatal("same input produced different fingerprints")
	}
	c := testSegment()
	c.Records[0].Offset = 999
	if InputFingerprint(a) == InputFingerprint(c) {
		t.Fatal("offset change did not alter the fingerprint")
	}
	d := testSegment()
	d.ID = 99
	if InputFingerprint(a) == InputFingerprint(d) {
		t.Fatal("segment id change did not alter the fingerprint")
	}
}

func TestCancellationStopsRender(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := (FrameRenderer{}).Render(ctx, testSegment()); err == nil {
		t.Fatal("cancelled render returned nil error")
	}
}
