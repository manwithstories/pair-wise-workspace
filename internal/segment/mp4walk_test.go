package segment

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/model"
)

func box(btype string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	putU32(out[:4], uint32(8+len(payload)))
	copy(out[4:8], btype)
	copy(out[8:], payload)
	return out
}

func putU32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
}

// tfhdPayload builds a FullBox tfhd with optional default_sample_flags.
func tfhdPayload(trackID uint32, flags uint32, defaultFlags *uint32) []byte {
	var p []byte
	p = append(p, 0, byte(flags>>16), byte(flags>>8), byte(flags)) // ver+flags
	p = append(p, byte(trackID>>24), byte(trackID>>16), byte(trackID>>8), byte(trackID))
	if defaultFlags != nil {
		p = append(p, byte((*defaultFlags)>>24), byte((*defaultFlags)>>16), byte((*defaultFlags)>>8), byte(*defaultFlags))
	}
	return p
}

func validFragment() []byte {
	var b []byte
	b = append(b, box("ftyp", []byte("iso5dash"))...)
	tfhd := box("tfhd", tfhdPayload(1, 0, nil))
	traf := box("traf", tfhd)
	moof := box("moof", traf)
	b = append(b, moof...)
	b = append(b, box("mdat", bytes.Repeat([]byte{0xAB}, 32))...)
	return b
}

func walkReader(t *testing.T, data []byte) (*WalkReport, error) {
	t.Helper()
	return WalkStream(bytes.NewReader(data), int64(len(data)), model.TrackStudio, 0)
}

func TestWalk_ValidFragment(t *testing.T) {
	data := validFragment()
	rep, err := walkReader(t, data)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if !rep.HasMoof || rep.TfhdCount != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if !rep.Sync() {
		t.Fatal("plain tfhd must be treated as sync")
	}
	if len(rep.Boxes) != 5 {
		t.Fatalf("want 5 boxes incl. nested traf/tfhd, got %d: %+v", len(rep.Boxes), rep.Boxes)
	}
	if rep.Boxes[3].Type != "tfhd" || rep.Boxes[3].Depth != 2 {
		t.Fatalf("nested tfhd record wrong: %+v", rep.Boxes[3])
	}
}

func TestWalk_BoxLengthOutOfRangeExactOffset(t *testing.T) {
	good := validFragment()
	// Declare an mdat 100 bytes larger than the fragment really is.
	bad := make([]byte, len(good))
	copy(bad, good)
	mdatOff := int64(len(good)) - 8 - 32
	putU32(bad[mdatOff:mdatOff+4], uint32(8+32+100))
	_, err := walkReader(t, bad)
	var me *model.MediaError
	if !errors.As(err, &me) {
		t.Fatalf("want MediaError, got %v", err)
	}
	if !errors.Is(err, model.ErrBoxOutOfRange) || !me.Recoverable {
		t.Fatalf("want recoverable ErrBoxOutOfRange, got %v", err)
	}
	if me.Offset != mdatOff {
		t.Fatalf("bad offset = %d, want %d (error budget 1 byte)", me.Offset, mdatOff)
	}
}

func TestWalk_TailTruncatedHeader(t *testing.T) {
	good := validFragment()
	truncated := append(append([]byte{}, good...), 0x00, 0x00, 0x01)
	_, err := walkReader(t, truncated)
	var me *model.MediaError
	if !errors.As(err, &me) || !errors.Is(err, model.ErrTruncated) {
		t.Fatalf("want ErrTruncated, got %v", err)
	}
	if !me.Recoverable {
		t.Fatal("tail truncation must be recoverable")
	}
	if me.Offset != int64(len(truncated)) {
		t.Fatalf("offset = %d, want %d", me.Offset, len(truncated))
	}
}

func TestWalk_BoxSizeBelowHeaderIsFatal(t *testing.T) {
	var b []byte
	b = append(b, box("ftyp", []byte("x"))...)
	b = append(b, 0x00, 0x00, 0x00, 0x04, 'm', 'd', 'a', 't')
	b = append(b, make([]byte, 16)...)
	_, err := walkReader(t, b)
	var me *model.MediaError
	if !errors.As(err, &me) || !errors.Is(err, model.ErrBadSegment) {
		t.Fatalf("want ErrBadSegment, got %v", err)
	}
	if me.Recoverable {
		t.Fatal("illegal box size 4 is not recoverable")
	}
}

func TestWalk_NonSyncTfhd(t *testing.T) {
	flags := uint32(0x000020)
	sampleFlags := uint32(0x01010000) // contains sample_is_non_sync_sample 0x00010000
	var b []byte
	b = append(b, box("moof", box("traf", box("tfhd", tfhdPayload(2, flags, &sampleFlags))))...)
	b = append(b, box("mdat", make([]byte, 8))...)
	rep, err := walkReader(t, b)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if rep.Sync() {
		t.Fatal("tfhd non-sync flag must make the fragment uncuttable")
	}
}

func TestValidate_ByteRangeOffsetsAreFileRelative(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blob.mp4")
	frag := validFragment()
	prefix := []byte("JUNKPREFIX1234") // 14 bytes of unrelated container prefix
	if err := os.WriteFile(path, append(prefix, frag...), 0o600); err != nil {
		t.Fatal(err)
	}
	seg := model.SegmentRef{Track: model.TrackField, Index: 3, URI: path,
		ByteStart: int64(len(prefix)), ByteSize: int64(len(frag)), Sync: true}
	ev, err := Validate(path, seg)
	if err != nil || ev == nil || !ev.Report.HasMoof {
		t.Fatalf("valid byte range must validate, got ev=%v err=%v", ev, err)
	}

	// Now truncate the underlying file: declared range exceeds real bytes.
	bad := append(prefix, frag[:len(frag)-10]...)
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Validate(path, seg)
	var me *model.MediaError
	if !errors.As(err, &me) {
		t.Fatalf("want MediaError, got %v", err)
	}
	// First missing byte is exactly the physical end of the truncated file.
	wantOffset := int64(len(bad))
	if abs(me.Offset-wantOffset) > 1 {
		t.Fatalf("offset = %d, want %d ±1", me.Offset, wantOffset)
	}
	if !me.Recoverable || !errors.Is(err, model.ErrTruncated) {
		t.Fatalf("want recoverable ErrTruncated, got %v", err)
	}
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
