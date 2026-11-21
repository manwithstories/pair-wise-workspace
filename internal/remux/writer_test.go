package remux

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"archivecheck.local/archivecheck/internal/fault"
)

func faultStage(err error) fault.Stage { return fault.StageOf(err) }

// failingSeeker fails writes after a limit, to prove the writer reports output
// failures instead of producing a short file.
type failingSeeker struct {
	memWriteSeeker
	limit int
}

func (f *failingSeeker) Write(p []byte) (int, error) {
	if len(f.buf)+len(p) > f.limit {
		return 0, io.ErrShortWrite
	}
	return f.memWriteSeeker.Write(p)
}

func TestWriterPropagatesOutputFailure(t *testing.T) {
	fs := &failingSeeker{limit: 64}
	_, err := Write(fs, testPlan(t))
	if err == nil {
		t.Fatal("expected an output failure")
	}
	if faultStage(err) == "" {
		t.Fatalf("failure carries no stage: %v", err)
	}
	if !errors.Is(err, fault.ErrOutput) {
		t.Fatalf("failure is not classified as an output error: %v", err)
	}
}

func TestWriterRejectsBadBoxType(t *testing.T) {
	out := &memWriteSeeker{}
	bw := &writer{w: out}
	if err := bw.box("abc", func() error { return nil }); err == nil {
		t.Fatal("short box type accepted")
	}
}

func TestWriterPatchesNestedSizes(t *testing.T) {
	out := &memWriteSeeker{}
	bw := &writer{w: out}
	if err := bw.box("moov", func() error {
		return bw.box("trak", func() error { return bw.write([]byte{1, 2, 3}) })
	}); err != nil {
		t.Fatal(err)
	}
	// moov size = 8 + (8 + 3) = 19; trak size = 11.
	if got := out.buf[0:4]; !bytes.Equal(got, []byte{0, 0, 0, 19}) {
		t.Fatalf("moov size=% x", got)
	}
	if got := out.buf[8:12]; !bytes.Equal(got, []byte{0, 0, 0, 11}) {
		t.Fatalf("trak size=% x", got)
	}
	if string(out.buf[4:8]) != "moov" || string(out.buf[12:16]) != "trak" {
		t.Fatalf("box types wrong: %q %q", out.buf[4:8], out.buf[12:16])
	}
}

func TestWriterBox64UsesLargeSize(t *testing.T) {
	out := &memWriteSeeker{}
	bw := &writer{w: out}
	if err := bw.box64("mdat", func() error { return bw.write([]byte{0xAA}) }); err != nil {
		t.Fatal(err)
	}
	if out.buf[0] != 0 || out.buf[1] != 0 || out.buf[2] != 0 || out.buf[3] != 1 {
		t.Fatalf("size field should be 1 for largesize: % x", out.buf[:4])
	}
	if string(out.buf[4:8]) != "mdat" {
		t.Fatalf("type=%q", out.buf[4:8])
	}
	if got := out.buf[8:16]; !bytes.Equal(got, []byte{0, 0, 0, 0, 0, 0, 0, 17}) {
		t.Fatalf("largesize=% x", got)
	}
}
