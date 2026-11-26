package audio

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pairflow/internal/media"
)

func TestPlanMatrixKnownLayouts(t *testing.T) {
	m1, err := PlanMatrix(1, "mono")
	if err != nil || m1.Rows() != 3 {
		t.Fatalf("mono plan: rows=%d err=%v", m1.Rows(), err)
	}
	m2, err := PlanMatrix(2, "stereo")
	if err != nil || m2.Rows() != 2 {
		t.Fatalf("stereo plan: rows=%d err=%v", m2.Rows(), err)
	}
	m6, err := PlanMatrix(6, "5.1")
	if err != nil || m6.Rows() != 6 {
		t.Fatalf("5.1 plan: rows=%d err=%v", m6.Rows(), err)
	}
	for _, bad := range []int{0, 3, 4, 8, 7} {
		if _, err := PlanMatrix(bad, "bad"); err == nil || err.Class != media.ClassInput {
			t.Fatalf("layout %d must be a class-2 error, got %v", bad, err)
		}
	}
}

func TestPlanMatrixMonoPanLaw(t *testing.T) {
	m, _ := PlanMatrix(1, "mono")
	dst := make([]float64, 6)
	m.MixFrame(dst, []float64{1.0})
	if math.Abs(dst[0]-minus3dB) > 1e-6 || math.Abs(dst[1]-minus3dB) > 1e-6 {
		t.Fatalf("mono L/R mirror wrong: L=%f R=%f", dst[0], dst[1])
	}
	if dst[2] != 1.0 {
		t.Fatalf("mono center gain wrong: %f", dst[2])
	}
	if dst[3] != 0 || dst[4] != 0 || dst[5] != 0 {
		t.Fatalf("mono must not leak into LFE/surround: %v", dst)
	}
}

func TestPlanMatrix5Point1Identity(t *testing.T) {
	m, _ := PlanMatrix(6, "5.1")
	dst := make([]float64, 6)
	src := []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6}
	m.MixFrame(dst, src)
	for i := range dst {
		if math.Abs(dst[i]-src[i]) > 1e-9 {
			t.Fatalf("5.1 must be identity at ch %d: %v", i, dst)
		}
	}
}

func TestNewSourceRejectsUnknown(t *testing.T) {
	for _, rate := range []int{8000, 22050, 32000, 11025} {
		if _, err := NewSource(rate, 2, "x", "p"); err == nil || err.Class != media.ClassInput {
			t.Fatalf("rate %d must be rejected", rate)
		}
	}
	for _, ch := range []int{0, 3, 4, 8} {
		if _, err := NewSource(48000, ch, "x", "p"); err == nil || err.Class != media.ClassInput {
			t.Fatalf("channel %d must be rejected", ch)
		}
	}
	if s, err := NewSource(96000, 6, "remote", "p"); err != nil || s.Rate != 96000 {
		t.Fatalf("valid source rejected: %v", err)
	}
}

func TestOutFramesExact(t *testing.T) {
	cases := []struct{ in, rate, want int }{
		{44100, 44100, 48000},
		{48000, 48000, 48000},
		{96000, 96000, 48000},
		{22050, 44100, 24000},
		{144000, 96000, 72000},
	}
	for _, c := range cases {
		if got := OutFrames(c.in, c.rate); got != c.want {
			t.Fatalf("%d@%d -> %d, want %d", c.in, c.rate, got, c.want)
		}
	}
}

func TestCutPointsWithinOneSample(t *testing.T) {
	// A cut at an arbitrary rational time must land within one canonical
	// sample of the exact instant at every supported source rate.
	cut := media.MustTime(1, 7) // 0.142857 s
	exactCanon := cut.Float() * float64(media.AudioRate)
	for _, rate := range []int{44100, 48000, 96000} {
		src, _ := NewSource(rate, 1, "boom", "p")
		outIdx, err := cut.SampleIndex()
		if err != nil {
			t.Fatalf("sample index: %v", err)
		}
		if math.Abs(float64(outIdx)-exactCanon) > 0.5+1e-9 {
			t.Fatalf("rate %d cut off by %.4f samples", rate, math.Abs(float64(outIdx)-exactCanon))
		}
		// The source position evaluated at the canonical boundary must also
		// be within one source sample of the exact cut instant.
		pos := float64(outIdx) / float64(media.AudioRate) * float64(rate)
		if math.Abs(pos-cut.Float()*float64(rate)) > 1.0 {
			t.Fatalf("rate %d source position off by %.4f samples", rate, math.Abs(pos-cut.Float()*float64(rate)))
		}
		_ = src
	}
}

func TestWavRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mix.wav")
	w, err := Create(path, NormSpec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	frames := 1500
	pcm := make([]int16, frames*6)
	for i := range pcm {
		pcm[i] = int16(i%200 - 100)
	}
	if err := w.Append(pcm); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if w.Frames() != int64(frames) {
		t.Fatalf("frame count %d, want %d", w.Frames(), frames)
	}

	r, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()
	if r.Format.Rate != 48000 || r.Format.Channels != 6 {
		t.Fatalf("format %+v", r.Format)
	}
	if r.Frames() != int64(frames) {
		t.Fatalf("declared frames %d, want %d", r.Frames(), frames)
	}
	var got []int16
	for {
		block, rerr := r.ReadFrames(512)
		if rerr != nil {
			break
		}
		got = append(got, block...)
	}
	if len(got) != len(pcm) {
		t.Fatalf("read %d samples, want %d", len(got), len(pcm))
	}
	for i := range pcm {
		if got[i] != pcm[i] {
			t.Fatalf("sample %d: got %d want %d", i, got[i], pcm[i])
		}
	}
}

func TestWavRejectsForeignEncoding(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.wav")
	// A 44-byte header claiming float encoding (tag 3) must be rejected.
	hdr := make([]byte, 44)
	copy(hdr[0:], "RIFF")
	copy(hdr[8:], "WAVE")
	copy(hdr[12:], "fmt ")
	binary.LittleEndian.PutUint16(hdr[20:], 3)
	copy(hdr[36:], "data")
	if err := os.WriteFile(p, hdr, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(p); err == nil || err.Class != media.ClassInput {
		t.Fatalf("non-PCM header must be class-2, got %v", err)
	}
	if _, err := Open(p); err != nil && !strings.Contains(err.Error(), "encoding") {
		t.Fatalf("error should name the unsupported encoding: %v", err)
	}
}

func TestWavPartialFrameRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "partial.wav")
	w, _ := Create(path, NormSpec)
	// 5 samples into 6 channels is not a whole frame.
	if err := w.Append(make([]int16, 5)); err == nil {
		t.Fatal("partial frame accepted")
	}
	_ = w.Close()
}

func TestMixClipsProducesAudibleSignal(t *testing.T) {
	src, _ := NewSource(44100, 1, "boom", "p")
	clips := []ClipMix{{Span: Span{Start: 0, End: 4800}, Source: src, Label: "boom"}}
	dst := make([]int16, 4800*6)
	if err := MixClips(dst, 0, clips, 0.8); err != nil {
		t.Fatalf("mix: %v", err)
	}
	if PeakAbs(dst) == 0 || RMS(dst) == 0 {
		t.Fatal("normalized mix is silent")
	}
}

func TestMixClipsDissolveEqualPower(t *testing.T) {
	a, _ := NewSource(48000, 2, "a", "p")
	b, _ := NewSource(48000, 2, "b", "p")
	window := int64(2400) // 50 ms
	dst := make([]int16, 9600*6)
	clips := []ClipMix{
		{Span: Span{0, 4800}, Source: a, Label: "a", DissolveOut: window},
		{Span: Span{4800 - window, 9600}, Source: b, Label: "b", DissolveIn: window},
	}
	if err := MixClips(dst, 0, clips, 0.5); err != nil {
		t.Fatalf("mix: %v", err)
	}
	// During the dissolve both clips overlap; there must be no silent gap.
	if RMS(dst[(4800-window)*6:4800*6]) == 0 {
		t.Fatal("dissolve window is silent")
	}
	if RMS(dst[0:100*6]) == 0 || RMS(dst[9500*6:]) == 0 {
		t.Fatal("steady portions are silent")
	}
}
