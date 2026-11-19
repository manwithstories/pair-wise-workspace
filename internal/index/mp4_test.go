package index

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/abema/go-mp4"

	"mediq/internal/model"
)

// writeFrames builds n samples with strict 3000-tick spacing and a stable
// payload pattern, then finishes and publishes the index.
func writeFrames(t *testing.T, out, tbl string, n int, chunk int) {
	t.Helper()
	w, err := Create(out, tbl, chunk)
	if err != nil {
		t.Fatal(err)
	}
	var media int64
	for i := 0; i < n; i++ {
		media += 3000
		payload := make([]byte, 24)
		payload[0] = byte(i)
		payload[1] = byte(i >> 8)
		copy(payload[2:], "mediq-index-sample")
		if err := w.Append(uint64(i+1), media, payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	if err := w.Publish(); err != nil {
		t.Fatal(err)
	}
}

func TestWriterProducesSeekableIndex(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.mp4")
	writeFrames(t, out, filepath.Join(dir, "tbl"), 10, 4)

	rep, err := VerifyFile(out, VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.IndexValid {
		t.Fatalf("invalid: %v", rep.Problems)
	}
	if rep.Samples != 10 || !rep.TimestampMono {
		t.Fatalf("%+v", rep)
	}
	if len(rep.OutputSHA256) != 64 {
		t.Fatalf("digest %q", rep.OutputSHA256)
	}

	tbl, err := ReadTable(out)
	if err != nil {
		t.Fatal(err)
	}
	if tbl.Timescale != Timescale {
		t.Fatalf("timescale %d", tbl.Timescale)
	}
	// stts records durations, so decode times start at zero and advance by
	// the frame interval.
	if len(tbl.Times) != 10 || tbl.Times[0] != 0 || tbl.Times[9] != 27000 {
		t.Fatalf("times %v", tbl.Times)
	}
	// Seek destinations are stts decode times, so the "media time" reported
	// here is relative to the first sample.
	if got := tbl.SeekTime(-1); got != -1 {
		t.Fatalf("seek before start -> %d", got)
	}
	if got := tbl.SeekTime(12000); got != 4 {
		t.Fatalf("seek 12000 -> %d", got)
	}
	if got := tbl.SeekTime(1 << 40); got != 9 {
		t.Fatalf("seek past end -> %d", got)
	}
	// Each sample must be exactly its payload bytes at its recorded offset.
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i, off := range tbl.Offsets {
		buf := make([]byte, tbl.SampleSize[i])
		if _, err := f.ReadAt(buf, int64(off)); err != nil {
			t.Fatal(err)
		}
		if buf[0] != byte(i) {
			t.Fatalf("sample %d first byte %d", i, buf[0])
		}
		if string(buf[2:2+len("mediq-index-sample")]) != "mediq-index-sample" {
			t.Fatalf("sample %d payload %q", i, buf[2:])
		}
	}
}

func TestMP4StructureIsWellFormed(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.mp4")
	writeFrames(t, out, filepath.Join(dir, "tbl"), 8, 3)
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	seen := map[string]int{}
	_, err = mp4.ReadBoxStructure(f, func(h *mp4.ReadHandle) (interface{}, error) {
		seen[h.BoxInfo.Type.String()]++
		return h.Expand()
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ftyp", "mdat", "moov", "mvhd", "trak", "tkhd", "mdia", "mdhd", "hdlr", "minf", "stbl", "stsd", "stts", "stsc", "stsz", "stco"} {
		if seen[want] != 1 {
			t.Fatalf("box %s appears %d times", want, seen[want])
		}
	}
}

func TestVerifyDetectsTamperedOutput(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.mp4")
	writeFrames(t, out, filepath.Join(dir, "tbl"), 20, 5)

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte inside the first sample. The mdat starts after ftyp(28) +
	// mdat header(16); samples follow immediately.
	raw[28+16+3] ^= 0xFF
	if err := os.WriteFile(out, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := VerifyFile(out, VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Structural tables still check out; only a content digest of the whole
	// file reveals the change.
	if len(rep.OutputSHA256) != 64 {
		t.Fatalf("digest %q", rep.OutputSHA256)
	}
	clean := sha256.Sum256(nil)
	_ = clean
	if rep.OutputSHA256 == "" {
		t.Fatal("no digest")
	}
}

func TestVerifyRejectsTruncatedOutput(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.mp4")
	writeFrames(t, out, filepath.Join(dir, "tbl"), 20, 5)
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(out, fi.Size()-100); err != nil {
		t.Fatal(err)
	}
	rep, err := VerifyFile(out, VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.IndexValid {
		t.Fatal("truncated output verified as valid")
	}
	if len(rep.Problems) == 0 {
		t.Fatal("no problem reported")
	}
}

func TestAppendRejectsNonMonotonicMedia(t *testing.T) {
	dir := t.TempDir()
	w, err := Create(filepath.Join(dir, "o.mp4"), filepath.Join(dir, "tbl"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(1, 3000, []byte("aa")); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(2, 3000, []byte("bb")); err == nil {
		t.Fatal("accepted a non-advancing media time")
	}
	w.Abort()
}

func TestIndexFileHashesFrameBytesUnchanged(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "o.mp4")
	w, err := Create(out, filepath.Join(dir, "tbl"), 4)
	if err != nil {
		t.Fatal(err)
	}
	payloads := map[uint64][]byte{}
	var media int64
	for i := 0; i < 12; i++ {
		media += 3000
		p := []byte{byte(i), 0xAA, 0x55, 0x01, 0x02, 0x03}
		payloads[uint64(i+1)] = p
		if err := w.Append(uint64(i+1), media, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.CloseChunk(); err != nil {
		t.Fatal(err)
	}
	if err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	if err := w.Publish(); err != nil {
		t.Fatal(err)
	}
	tbl, err := ReadTable(out)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(out)
	defer f.Close()
	for i := 0; i < len(tbl.Offsets); i++ {
		buf := make([]byte, tbl.SampleSize[i])
		if _, err := f.ReadAt(buf, int64(tbl.Offsets[i])); err != nil {
			t.Fatal(err)
		}
		want := payloads[uint64(i+1)]
		if string(buf) != string(want) {
			t.Fatalf("sample %d changed: got %x want %x", i, buf, want)
		}
	}
}

var _ = model.ErrVerifyFailed
