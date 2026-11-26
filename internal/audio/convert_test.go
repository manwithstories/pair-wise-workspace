package audio

import (
	"math"
	"testing"

	"mediamix/internal/clock"
)

// sineS16 fills n mono s16 samples of a 1 kHz tone at the given rate.
func sineS16(t *testing.T, n, rate int, hz float64) []byte {
	t.Helper()
	buf := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := int16(20000 * math.Sin(2*math.Pi*hz*float64(i)/float64(rate)))
		buf[2*i] = byte(v)
		buf[2*i+1] = byte(v >> 8)
	}
	return buf
}

func TestConvertRejectsBadPayload(t *testing.T) {
	c, err := NewConverter(DefaultSource(), DefaultTarget())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Flush()
	if _, err := c.Convert([]byte{1, 2, 3}); !clock.IsCode(err, clock.ErrInvalidPacket) {
		t.Fatalf("odd-length payload should be rejected, got %v", err)
	}
	if _, err := c.Convert(nil); !clock.IsCode(err, clock.ErrInvalidPacket) {
		t.Fatalf("empty payload should be rejected, got %v", err)
	}
}

func TestConvertBoundarySampleAccuracy(t *testing.T) {
	c, err := NewConverter(DefaultSource(), DefaultTarget())
	if err != nil {
		t.Fatal(err)
	}
	// Feed the same 4800-sample tone as one block and as 15 fragments; both
	// must yield exactly 14400 stereo frames (4800 * 48000/16000).
	const inSamples = 4800
	whole := sineS16(t, inSamples, 16000, 1000)

	one, err := c.Convert(whole)
	if err != nil {
		t.Fatalf("whole convert: %v", err)
	}
	flushed, err := c.Flush()
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	one = append(one, flushed...)
	if len(one)/2 != 14400 {
		t.Fatalf("whole: got %d frames, want 14400", len(one)/2)
	}
	inN, outN := c.Totals()
	if inN != inSamples || outN != 14400 || outN != c.ExpectedFrames() {
		t.Fatalf("totals mismatch in=%d out=%d expected=%d", inN, outN, c.ExpectedFrames())
	}

	c2, _ := NewConverter(DefaultSource(), DefaultTarget())
	var frag []float32
	for off := 0; off < inSamples; off += 320 {
		part, err := c2.Convert(whole[off*2 : (off+320)*2])
		if err != nil {
			t.Fatalf("fragment convert: %v", err)
		}
		frag = append(frag, part...)
	}
	tail, err := c2.Flush()
	if err != nil {
		t.Fatalf("fragment flush: %v", err)
	}
	frag = append(frag, tail...)
	if len(frag) != len(one) {
		t.Fatalf("fragment samples %d != whole samples %d (lost or repeated at boundary)",
			len(frag), len(one))
	}
	for i := range one {
		if one[i] != frag[i] {
			t.Fatalf("sample %d differs: whole=%v frag=%v", i, one[i], frag[i])
		}
	}
}

func TestConvertStereoContent(t *testing.T) {
	c, _ := NewConverter(DefaultSource(), DefaultTarget())
	out, err := c.Convert(sineS16(t, 320, 16000, 440))
	if err != nil {
		t.Fatal(err)
	}
	// The first fragment leaves swresample's 48-sample filter delay buffered;
	// it is emitted at flush, bringing the total to exactly 960 frames.
	tail, err := c.Flush()
	if err != nil {
		t.Fatal(err)
	}
	totalFrames := (len(out) + len(tail)) / 2
	if totalFrames != 960 {
		t.Fatalf("320 in -> want 960 stereo frames after flush, got %d", totalFrames)
	}
	// Mono up-mix: both channels must carry identical content.
	for i := 0; i+1 < len(out); i += 2 {
		if out[i] != out[i+1] {
			t.Fatalf("stereo channels differ at frame %d: %v != %v", i/2, out[i], out[i+1])
		}
	}
}

func TestConvertArbitraryFragmentSizes(t *testing.T) {
	c, _ := NewConverter(DefaultSource(), DefaultTarget())
	sizes := []int{100, 37, 512, 1, 640, 77}
	var total int64
	for _, n := range sizes {
		out, err := c.Convert(sineS16(t, n, 16000, 300))
		if err != nil {
			t.Fatalf("size %d: %v", n, err)
		}
		total += int64(len(out) / 2)
	}
	tail, _ := c.Flush()
	total += int64(len(tail) / 2)
	in, _ := c.Totals()
	if total != c.ExpectedFrames() || in != int64(sum(sizes)) {
		t.Fatalf("arbitrary fragments lost samples: out=%d expected=%d in=%d",
			total, c.ExpectedFrames(), in)
	}
}

func sum(xs []int) int {
	n := 0
	for _, x := range xs {
		n += x
	}
	return n
}
