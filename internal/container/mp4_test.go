package container

import (
	"os"
	"path/filepath"
	"testing"

	"mediacore/internal/media"
)

func openPath(t *testing.T, path string) (*Source, *os.File) {
	t.Helper()
	adapter := NewMP4(DefaultOptions())
	src, f, err := adapter.OpenFile(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { f.Close() })
	return src, f
}

func writeTemp(t *testing.T, spec FixtureSpec) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f.mp4")
	if err := WriteFixture(path, spec); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return path
}

// TestParseFixture checks the sample table expansion against the fixture.
func TestParseFixture(t *testing.T) {
	path := writeTemp(t, FixtureSpec{Rate: 44100, Channels: 2, Frames: 44100, FrameSize: 1024})
	src, _ := openPath(t, path)
	tr, ok := src.Track(1)
	if !ok {
		t.Fatal("track 1 missing")
	}
	if tr.Timescale != 44100 || tr.SampleRate != 44100 || tr.Channels != 2 {
		t.Fatalf("track = %+v", tr)
	}
	// 44100 frames split into 1024-frame samples: 44 samples, last short.
	if tr.SampleCount != 44 {
		t.Fatalf("samples = %d, want 44", tr.SampleCount)
	}
	cur, err := src.Cursor(1)
	if err != nil {
		t.Fatal(err)
	}
	defer cur.Close()
	var total int64
	var lastDTS int64
	for {
		s, err := cur.Next()
		if err != nil {
			break
		}
		total += int64(s.Size)
		if s.DTS < lastDTS {
			t.Fatalf("dts regressed at %d", s.Index)
		}
		lastDTS = s.DTS
		if _, ok := src.CoveringMdat(s.Offset, int64(s.Size)); !ok {
			t.Fatalf("sample %d outside mdat", s.Index)
		}
	}
	if total != 44100*2*2 {
		t.Fatalf("total bytes %d, want %d", total, 44100*2*2)
	}
}

// TestTruncatedMdatOffset locates the first missing byte exactly.
func TestTruncatedMdatOffset(t *testing.T) {
	path := writeTemp(t, FixtureSpec{Rate: 48000, Channels: 2, Frames: 48000, FrameSize: 4096})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	idx := indexOfBox(t, data, "mdat")
	payload := idx + 8
	cut := payload + (len(data)-payload)/2
	trunc := filepath.Join(t.TempDir(), "t.mp4")
	if err := os.WriteFile(trunc, data[:cut], 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err = NewMP4(DefaultOptions()).OpenFile(trunc)
	if err == nil {
		t.Fatal("expected truncation error")
	}
	me, ok := media.AsError(err)
	if !ok || me.Code != media.CodeTruncatedMdat {
		t.Fatalf("got %v", err)
	}
	if diff := me.Offset - int64(cut); diff < -1 || diff > 1 {
		t.Fatalf("offset %d, truncation at %d (diff %d)", me.Offset, cut, diff)
	}
}

// TestInvalidBoxLengthOffset locates the offending size field.
func TestInvalidBoxLengthOffset(t *testing.T) {
	path := writeTemp(t, FixtureSpec{Rate: 48000, Channels: 1, Frames: 4800, FrameSize: 512})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	idx := indexOfBox(t, data, "moov")
	// Declare a size beyond the file.
	data[idx], data[idx+1], data[idx+2], data[idx+3] = 0x7f, 0xff, 0xff, 0xff
	bad := filepath.Join(t.TempDir(), "b.mp4")
	if err := os.WriteFile(bad, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err = NewMP4(DefaultOptions()).OpenFile(bad)
	if err == nil {
		t.Fatal("expected length error")
	}
	me, ok := media.AsError(err)
	if !ok {
		t.Fatalf("got %v", err)
	}
	if me.Code != media.CodeInvalidBoxLength && me.Code != media.CodeOversizedBox {
		t.Fatalf("code %s", me.Code)
	}
	if me.Offset != int64(idx) {
		t.Fatalf("offset %d, want %d", me.Offset, idx)
	}
}

// TestTimestampRegressionOffset proves a regressing ctts is attributed to the
// sample that carries it.
func TestTimestampRegressionOffset(t *testing.T) {
	path := writeTemp(t, FixtureSpec{Rate: 48000, Channels: 2, Frames: 4800, FrameSize: 512, CTTSTrick: true})
	src, _ := openPath(t, path)
	cur, err := src.Cursor(1)
	if err != nil {
		t.Fatal(err)
	}
	defer cur.Close()
	var regressed *media.Error
	for {
		_, err := cur.Next()
		if err == nil {
			continue
		}
		if me, ok := media.AsError(err); ok && me.Code == media.CodeTimestampRegression {
			regressed = me
		}
		break
	}
	if regressed == nil {
		t.Fatal("no regression reported")
	}
	if regressed.Track != 1 || !regressed.HasOffset {
		t.Fatalf("attribution missing: %+v", regressed)
	}
}

// TestSampleOutsideFile reports the truncation point for a chunk offset past
// the end of the file.
func TestSampleOutsideFile(t *testing.T) {
	path := writeTemp(t, FixtureSpec{Rate: 48000, Channels: 1, Frames: 4800, FrameSize: 512})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture has one chunk per sample at a fixed stride; corrupt the
	// final chunk offset to point past EOF by rewriting stco's last entry.
	cut := len(data) - 100
	trunc := filepath.Join(t.TempDir(), "c.mp4")
	if err := os.WriteFile(trunc, data[:cut], 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err = NewMP4(DefaultOptions()).OpenFile(trunc)
	if err == nil {
		t.Fatal("expected error")
	}
	if me, ok := media.AsError(err); !ok || me.Code != media.CodeTruncatedMdat {
		t.Fatalf("got %v", err)
	}
}

// TestRejectsOversizedMetadata keeps a hostile declared size from becoming an
// allocation.
func TestRejectsOversizedMetadata(t *testing.T) {
	path := writeTemp(t, FixtureSpec{Rate: 48000, Channels: 1, Frames: 4800, FrameSize: 512})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	idx := indexOfBox(t, data, "moov")
	// A 1 GiB declaration with a matching (enormous) file is impossible
	// to build here, so the size check fires on the declaration alone.
	data[idx], data[idx+1], data[idx+2], data[idx+3] = 0x40, 0x00, 0x00, 0x00
	bad := filepath.Join(t.TempDir(), "o.mp4")
	if err := os.WriteFile(bad, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err = NewMP4(DefaultOptions()).OpenFile(bad)
	if err == nil {
		t.Fatal("expected oversized rejection")
	}
	me, _ := media.AsError(err)
	if me == nil || me.Code != media.CodeOversizedBox {
		t.Fatalf("got %v", err)
	}
}

// TestSampleMustLiveInsideMdat rejects a chunk table that points at structure
// bytes even though they are inside the file.
func TestSampleMustLiveInsideMdat(t *testing.T) {
	path := writeTemp(t, FixtureSpec{Rate: 48000, Channels: 1, Frames: 4800, FrameSize: 512})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Find the first stco entry and point it at the file start (inside
	// ftyp/moov, never inside mdat).
	idx := indexOfBox(t, data, "stco")
	// Layout: size(4) type(4) version+flags(4) count(4) first offset(4).
	data[idx+16], data[idx+17], data[idx+18], data[idx+19] = 0, 0, 0, 8
	bad := filepath.Join(t.TempDir(), "m.mp4")
	if err := os.WriteFile(bad, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err = NewMP4(DefaultOptions()).OpenFile(bad)
	if err == nil {
		t.Fatal("sample outside mdat accepted")
	}
	me, _ := media.AsError(err)
	if me == nil || me.Code != media.CodeSampleBeyondMdat || me.Track != 1 {
		t.Fatalf("got %v", err)
	}
}

func indexOfBox(t *testing.T, data []byte, tag string) int {
	t.Helper()
	for i := 4; i+4 <= len(data); i++ {
		if string(data[i:i+4]) == tag {
			size := int(data[i-4])<<24 | int(data[i-3])<<16 | int(data[i-2])<<8 | int(data[i-1])
			if size >= 8 && i-4+size <= len(data) {
				return i - 4
			}
		}
	}
	t.Fatalf("box %q not found", tag)
	return -1
}
