package mp4

import (
	"os"
	"path/filepath"
	"testing"

	"medialine/internal/fault"
	"medialine/internal/model"
	"medialine/internal/testkit"
)

func genClip(t *testing.T, tracks ...testkit.TrackSpec) (*testkit.Clip, string) {
	t.Helper()
	dir := t.TempDir()
	spec := testkit.ClipSpec{Name: "clip", Seed: 17, Tracks: tracks}
	c, err := testkit.GenerateClip(dir, spec)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return c, dir
}

func TestParseBuildsTracks(t *testing.T) {
	c, _ := genClip(t,
		testkit.DefaultVideoTrack(1, 4_000_000, 30),
		testkit.DefaultAudioTrack(2, 4_000_000))
	f, err := Parse(c.Path, Options{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(f.Issues) != 0 {
		t.Fatalf("unexpected issues: %v", f.Issues)
	}
	if len(f.Tracks) != 2 {
		t.Fatalf("tracks=%d", len(f.Tracks))
	}
	video := f.Tracks[0]
	if video.ID != 1 || video.Kind != model.KindVideo {
		t.Fatalf("unexpected video track: %+v", video)
	}
	if len(video.Samples) != 120 {
		t.Fatalf("video samples=%d", len(video.Samples))
	}
	if !video.Samples[0].Keyframe || video.Samples[1].Keyframe {
		t.Fatal("GOP boundaries wrong")
	}
	// Sample offsets must fall inside mdat.
	for _, s := range video.Samples[:5] {
		if uint64(s.Offset) < f.Mdat.Offset || uint64(s.Offset)+uint64(s.Size) > f.Mdat.End() {
			t.Fatalf("sample offset %d outside mdat", s.Offset)
		}
		ok, err := testkit.MarkerObserved(c.Path, s.Offset)
		if err != nil || !ok {
			t.Fatalf("marker missing at %d: %v", s.Offset, err)
		}
	}
	audio := f.Tracks[1]
	if audio.Kind != model.KindAudio || !audio.Samples[0].Keyframe {
		t.Fatalf("unexpected audio track: %+v", audio)
	}
}

func TestParseTruncatedTailReportsOffset(t *testing.T) {
	c, _ := genClip(t, testkit.DefaultVideoTrack(1, 2_000_000, 30))
	if err := testkit.CorruptTail(c.Path, 40); err != nil {
		t.Fatal(err)
	}
	f, err := Parse(c.Path, Options{})
	if err == nil {
		if len(f.Issues) == 0 {
			t.Fatal("expected a fault for truncated tail")
		}
		return
	}
	fe, ok := fault.As(err)
	if !ok {
		t.Fatalf("expected fault, got %v", err)
	}
	if fe.Kind != fault.KindTruncatedBox && fe.Kind != fault.KindMissingIndex {
		t.Fatalf("unexpected kind %s", fe.Kind)
	}
	if fe.Offset < 0 {
		t.Fatalf("fault must carry an offset: %v", fe)
	}
}

func TestParseMissingMoovIsRecoverable(t *testing.T) {
	c, _ := genClip(t, testkit.DefaultVideoTrack(1, 2_000_000, 30))
	// Rewrite the moov type so the scan ends without finding it.
	f, err := os.OpenFile(c.Path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	parsed, err := Parse(c.Path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("skip"), int64(parsed.Moov.Offset)+4); err != nil {
		t.Fatal(err)
	}
	f2, err := Parse(c.Path, Options{})
	if err != nil {
		t.Fatalf("missing moov should be an issue, not an error: %v", err)
	}
	if len(f2.Issues) == 0 {
		t.Fatal("expected a missing-index issue")
	}
	found := false
	for _, is := range f2.Issues {
		if is.Kind == fault.KindMissingIndex {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a missing-index issue, got %+v", f2.Issues)
	}
}

func TestParseOversizedBoxRejected(t *testing.T) {
	c, _ := genClip(t, testkit.DefaultVideoTrack(1, 2_000_000, 30))
	// Corrupt the mdat declaration; because moov is still resynchronisable the
	// parse recovers and reports the length fault as an issue.
	if err := testkit.CorruptLength(c.Path, c.MdatOffset, 0x7ffffff0); err != nil {
		t.Fatal(err)
	}
	f, err := Parse(c.Path, Options{})
	if err != nil {
		fe, _ := fault.As(err)
		if fe.Kind != fault.KindInvalidLength {
			t.Fatalf("expected invalid length, got %v", err)
		}
		return
	}
	var found bool
	for _, is := range f.Issues {
		if is.Kind == fault.KindInvalidLength {
			found = true
			if is.Offset < 0 {
				t.Fatalf("fault must carry an offset: %v", is)
			}
		}
	}
	if !found {
		t.Fatalf("expected invalid-length issue, got %+v", f.Issues)
	}
	if len(f.Tracks) == 0 {
		t.Fatal("undamaged tracks must still be parsed")
	}
}

func TestParseBoxSizeLimit(t *testing.T) {
	c, _ := genClip(t, testkit.DefaultVideoTrack(1, 2_000_000, 30))
	_, err := Parse(c.Path, Options{Limits: Limits{MaxBoxSize: 64}})
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindInvalidLength {
		t.Fatalf("expected limit fault, got %v", err)
	}
}

func TestParseCorruptedIndexFails(t *testing.T) {
	c, _ := genClip(t, testkit.DefaultVideoTrack(1, 2_000_000, 30))
	f, err := Parse(c.Path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Overwrite stsz payload with a huge sample count.
	fh, err := os.OpenFile(c.Path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	path := filepath.Clean(c.Path)
	if path == "" {
		t.Fatal("empty path")
	}
	stszOff := findBox(t, fh, f.Moov.Offset, f.Moov.Size, "stsz")
	var huge = []byte{0x00, 0x3f, 0xff, 0xff}
	if _, err := fh.WriteAt(huge, int64(stszOff)+16); err != nil {
		t.Fatal(err)
	}
	f2, err := Parse(c.Path, Options{})
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if len(f2.Issues) == 0 {
		t.Fatal("expected an issue for the corrupt stsz")
	}
	fe, _ := fault.As(f2.Issues[0])
	if fe.TrackID != 1 {
		t.Fatalf("fault should name track 1: %v", fe)
	}
}

// findBox recursively searches the box region [start,start+size) for the
// named box, descending into containers.
func findBox(t *testing.T, fh *os.File, start, size uint64, name string) uint64 {
	t.Helper()
	buf := make([]byte, 8)
	for off := start; off+8 <= start+size; {
		if _, err := fh.ReadAt(buf, int64(off)); err != nil {
			t.Fatalf("findBox %s: %v", name, err)
		}
		bsize := uint64(buf[0])<<24 | uint64(buf[1])<<16 | uint64(buf[2])<<8 | uint64(buf[3])
		if bsize < 8 || off+bsize > start+size {
			t.Fatalf("findBox %s: bad size %d at %d", name, bsize, off)
		}
		if string(buf[4:8]) == name {
			return off
		}
		switch string(buf[4:8]) {
		case "moov", "trak", "mdia", "minf", "stbl", "edts", "dinf":
			if got := findBoxMaybe(t, fh, off+8, bsize-8, name); got != 0 {
				return got
			}
		}
		off += bsize
	}
	return 0
}

func findBoxMaybe(t *testing.T, fh *os.File, start, size uint64, name string) uint64 {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("findBoxMaybe: %v", r)
		}
	}()
	buf := make([]byte, 8)
	for off := start; off+8 <= start+size; {
		if _, err := fh.ReadAt(buf, int64(off)); err != nil {
			return 0
		}
		bsize := uint64(buf[0])<<24 | uint64(buf[1])<<16 | uint64(buf[2])<<8 | uint64(buf[3])
		if bsize < 8 || off+bsize > start+size {
			return 0
		}
		if string(buf[4:8]) == name {
			return off
		}
		switch string(buf[4:8]) {
		case "moov", "trak", "mdia", "minf", "stbl", "edts", "dinf":
			if got := findBoxMaybe(t, fh, off+8, bsize-8, name); got != 0 {
				return got
			}
		}
		off += bsize
	}
	return 0
}

func TestFileClose(t *testing.T) {
	f := &File{}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
