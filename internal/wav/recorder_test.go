package wav

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"avpipe/internal/media"
)

// fileWriteSeeker is a temp-file-backed io.WriteSeeker + io.ReaderAt used to
// exercise the recorder's disk read-back without a real output directory.
func newFileWriteSeeker(t *testing.T, dir, name string) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestRecorderRoundTripExact(t *testing.T) {
	dir := t.TempDir()
	f := newFileWriteSeeker(t, dir, "out.wav")
	rec := NewRecorder(f, rate48, 2)
	samples := make([]float64, 0, 800)
	for i := 0; i < 200; i++ {
		v := 0.3 * math.Sin(float64(i)*0.05)
		samples = append(samples, v, -v)
	}
	b := media.Block{Track: "t", Start: 0, Rate: rate48, Channels: 2, Samples: samples}
	if err := rec.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rec.VerifyError(); err != nil {
		t.Fatal(err)
	}
	matched, mismatch := rec.RoundTrip()
	if mismatch != 0 || matched != 200 {
		t.Fatalf("matched=%d mismatch=%d", matched, mismatch)
	}
	if rec.Frames() != 200 {
		t.Fatalf("frames=%d", rec.Frames())
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	info, err := f.Stat()
	if err != nil || info.Size() <= 44 {
		t.Fatalf("output too small: %v size=%d", err, info.Size())
	}
	f.Close()
}

func TestRecorderRejectsNonContiguousAndFormat(t *testing.T) {
	dir := t.TempDir()
	f := newFileWriteSeeker(t, dir, "out.wav")
	defer f.Close()
	rec := NewRecorder(f, rate48, 1)
	blk := func(start int64, rate media.Rate, channels int) media.Block {
		return media.Block{Track: "t", Start: start, Rate: rate, Channels: channels, Samples: make([]float64, 10*channels)}
	}
	if err := rec.Write(blk(0, rate48, 1)); err != nil {
		t.Fatal(err)
	}
	if err := rec.Write(blk(5, rate48, 1)); err == nil {
		t.Fatal("overlap accepted")
	}
	if err := rec.Write(blk(10, rate48, 2)); err == nil {
		t.Fatal("channel change accepted")
	}
	if err := rec.Write(blk(10, media.Rate{Num: 44100, Den: 1}, 1)); err == nil {
		t.Fatal("rate change accepted")
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rec.Write(blk(10, rate48, 1)); err == nil {
		t.Fatal("write after close accepted")
	}
}

func TestRecorderDetectsCorruption(t *testing.T) {
	// A writer that silently mangles one byte must be caught by the read-back
	// verification, proving the round-trip metric is not vacuous.
	dir := t.TempDir()
	f := newFileWriteSeeker(t, dir, "out.wav")
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	cw := &corruptingFile{File: f, corruptAt: 1000}
	rec := NewRecorder(cw, rate48, 1)
	samples := make([]float64, 600)
	for i := range samples {
		samples[i] = 0.5
	}
	b := media.Block{Track: "t", Start: 0, Rate: rate48, Channels: 1, Samples: samples}
	if err := rec.Write(b); err != nil {
		t.Fatal(err)
	}
	matched, mismatch := rec.RoundTrip()
	if mismatch == 0 {
		t.Fatalf("corruption not detected (matched=%d)", matched)
	}
	if matched+mismatch != 600 {
		t.Fatalf("matched=%d mismatch=%d", matched, mismatch)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// corruptingFile flips one byte of the payload the first time it is written.
type corruptingFile struct {
	*os.File
	corruptAt int64
	done      bool
	written   int64
}

func (c *corruptingFile) Write(p []byte) (int, error) {
	if !c.done && c.written+int64(len(p)) > c.corruptAt {
		off := int(c.corruptAt - c.written)
		if off >= 0 && off < len(p) {
			p[off] ^= 0xff
			c.done = true
		}
	}
	n, err := c.File.Write(p)
	c.written += int64(n)
	return n, err
}

func TestRecorderWithoutReaderAtSkips(t *testing.T) {
	dir := t.TempDir()
	f := newFileWriteSeeker(t, dir, "out.wav")
	defer f.Close()
	ws := writeSeekerOnly{f}
	rec := NewRecorder(ws, rate48, 1)
	if err := rec.Write(media.Block{Track: "t", Start: 0, Rate: rate48, Channels: 1, Samples: make([]float64, 10)}); err != nil {
		t.Fatal(err)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	matched, mismatch := rec.RoundTrip()
	if matched != 0 || mismatch != 0 {
		t.Fatalf("expected skipped verification, got %d/%d", matched, mismatch)
	}
}

// writeSeekerOnly hides the ReaderAt method so verification is skipped.
type writeSeekerOnly struct{ f *os.File }

func (w writeSeekerOnly) Write(p []byte) (int, error)         { return w.f.Write(p) }
func (w writeSeekerOnly) Seek(o int64, w2 int) (int64, error) { return w.f.Seek(o, w2) }
