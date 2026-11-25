package container

import (
	"encoding/binary"
	"strings"
	"testing"

	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/fixture"
	"relay.local/relay/internal/metadata"
	"relay.local/relay/internal/model"
)

// buildTestMP4 returns a small valid container with one video track whose
// frames are numeric payloads, avoiding JPEG work in parser tests.
func buildTestMP4(t *testing.T) []byte {
	t.Helper()
	samples := []fixture.SampleSpec{
		{Payload: []byte("aaa"), Duration: 1001, Sync: true},
		{Payload: []byte("bbbb"), Duration: 1001, Sync: false},
		{Payload: []byte("ccccc"), Duration: 2002, Sync: true},
	}
	data, err := fixture.BuildMP4(fixture.MP4Spec{Tracks: []fixture.MP4TrackSpec{{
		ID: 1, Kind: model.Video, FourCC: "mjpg", Timescale: 30000, Width: 16, Height: 16,
		Rotation: 90,
		ColrBox:  &fixture.ColrSpec{Primaries: 1, Transfer: 1, Matrix: 1},
		Samples:  samples,
	}}})
	if err != nil {
		t.Fatalf("build fixture: %v", err)
	}
	return data
}

func TestParseMP4SampleTable(t *testing.T) {
	data := buildTestMP4(t)
	f, err := ParseMP4("t.mp4", data, DefaultLimits())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.Faults.Len() != 0 {
		t.Fatalf("unexpected faults: %v", f.Faults)
	}
	if len(f.Tracks) != 1 {
		t.Fatalf("tracks = %d, want 1", len(f.Tracks))
	}
	tr := f.Tracks[0]
	if tr.ID != 1 || tr.Kind != model.Video || tr.Codec != model.CodecMJPEG {
		t.Fatalf("track identity = %d/%s/%s", tr.ID, tr.Kind, tr.Codec)
	}
	if len(tr.Samples) != 3 {
		t.Fatalf("samples = %d, want 3", len(tr.Samples))
	}
	want := []struct {
		dts, dur int64
		sync     bool
		size     int64
	}{
		{0, 1001, true, 3},
		{1001, 1001, false, 4},
		{2002, 2002, true, 5},
	}
	for i, w := range want {
		s := tr.Samples[i]
		if s.DTS != w.dts || s.Duration != w.dur || s.Sync != w.sync || s.Size != w.size {
			t.Errorf("sample %d = %+v, want dts=%d dur=%d sync=%v size=%d",
				i, s, w.dts, w.dur, w.sync, w.size)
		}
	}
	// Payload access must recover the exact bytes the fixture wrote.
	got, err := tr.Payload(f.Data, tr.Samples[1])
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if string(got) != "bbbb" {
		t.Fatalf("payload = %q, want bbbb", got)
	}
}

func TestParseMP4RejectsTruncatedBoxWithOffset(t *testing.T) {
	data := buildTestMP4(t)
	moov := findBox(t, data, "moov")
	// Lie about the moov size and cut the tail so it cannot be satisfied.
	bad := append([]byte(nil), data...)
	binary.BigEndian.PutUint32(bad[moov:moov+4], uint32(len(bad)))
	bad = bad[:len(bad)-8]
	_, err := ParseMP4("bad.mp4", bad, DefaultLimits())
	fe, ok := fault.As(err)
	if !ok {
		t.Fatalf("expected fault, got %v", err)
	}
	if fe.Kind != fault.KindTruncated {
		t.Fatalf("kind = %s, want truncated", fe.Kind)
	}
	if fe.Offset != int64(moov) {
		t.Fatalf("offset = %d, want %d", fe.Offset, moov)
	}
	if fe.Recoverable {
		t.Fatal("truncated box must be fatal, not silently accepted")
	}
}

func TestParseMP4RejectsOversizeSampleOffset(t *testing.T) {
	data := buildTestMP4(t)
	// Corrupt the chunk offset table so a sample points past the file end.
	at := findNestedBox(t, data, "stco")
	bad := append([]byte(nil), data...)
	// stco: size(4) type(4) version/flags(4) count(4) then first offset.
	binary.BigEndian.PutUint32(bad[at+16:at+20], uint32(len(bad)+64))
	f, err := ParseMP4("bad.mp4", bad, DefaultLimits())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	found := false
	for _, it := range f.Faults.Sorted() {
		if it.Kind == fault.KindOutOfRange && it.Track == 0 && it.Recoverable {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an out-of-range sample fault, got %v", f.Faults)
	}
}

func TestParseMP4RoundTrip(t *testing.T) {
	data := buildTestMP4(t)
	f, err := ParseMP4("t.mp4", data, DefaultLimits())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Resolve the description so the writer emits effective values rather
	// than raw claims, matching how the pipeline uses it.
	resolveTrack(f.Tracks[0])
	orig := f.Tracks[0].FieldTable()
	out, err := WriteMP4(f)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	f2, err := ParseMP4("t2.mp4", out, DefaultLimits())
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	resolveTrack(f2.Tracks[0])
	got := f2.Tracks[0].FieldTable()
	if len(got) != len(orig) {
		t.Fatalf("field count after round trip = %d, want %d", len(got), len(orig))
	}
	for i := range got {
		if got[i] != orig[i] {
			t.Errorf("field %d differs: %q -> %q", i, orig[i], got[i])
		}
	}
	// Sample payloads must survive the rewrite byte for byte.
	for i := range f.Tracks[0].Samples {
		a, _ := f.Tracks[0].Payload(f.Data, f.Tracks[0].Samples[i])
		b, err := f2.Tracks[0].Payload(f2.Data, f2.Tracks[0].Samples[i])
		if err != nil {
			t.Fatalf("payload %d: %v", i, err)
		}
		if string(a) != string(b) {
			t.Fatalf("payload %d changed: %q -> %q", i, a, b)
		}
	}
}

func TestParseMXFRoundTrip(t *testing.T) {
	frames := [][]byte{[]byte("one"), []byte("two"), []byte("three")}
	data, err := fixture.BuildMXF(fixture.MXFSpec{Tracks: []fixture.MXFTrackSpec{{
		ID: 9, Frames: frames, Duration: 3,
		Durations: []uint32{10, 20, 30},
		Keys:      []bool{true, false, true},
		Container: map[uint16]string{0x8015: "16", 0x8016: "16", 0x8007: "180"},
		Bitstream: map[uint16]string{0x8019: "12x12", 0x8008: "90"},
	}}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	f, err := ParseMXF("t.mxf", data, DefaultLimits())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.Faults.Len() != 0 {
		t.Fatalf("faults: %v", f.Faults)
	}
	if len(f.Tracks) != 1 || len(f.Tracks[0].Samples) != 3 {
		t.Fatalf("tracks/samples = %d/%d", len(f.Tracks), len(f.Tracks[0].Samples))
	}
	if got := f.Tracks[0].Samples[2].Duration; got != 30 {
		t.Fatalf("index duration = %d, want 30", got)
	}
	orig := f.Tracks[0].FieldTable()
	out, err := WriteMXF(f)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	f2, err := ParseMXF("t2.mxf", out, DefaultLimits())
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if got := f2.Tracks[0].FieldTable(); len(got) != len(orig) {
		t.Fatalf("field count after round trip = %d, want %d", len(got), len(orig))
	}
	for i := range f.Tracks[0].Samples {
		a, _ := f.Tracks[0].Payload(f.Data, f.Tracks[0].Samples[i])
		b, err := f2.Tracks[0].Payload(f2.Data, f2.Tracks[0].Samples[i])
		if err != nil {
			t.Fatalf("payload %d: %v", i, err)
		}
		if string(a) != string(b) {
			t.Fatalf("payload %d changed: %q -> %q", i, a, b)
		}
	}
}

func TestParseMXFRejectsOversizeKLV(t *testing.T) {
	header := fixture.MXFHeaderForTest()
	essence := fixture.MXFEssenceKeyForTest()
	data := append([]byte(nil), header...)
	data = append(data, 0x00) // empty partition value
	data = append(data, essence...)
	// Long-form BER length of 2^31, far beyond the buffer.
	data = append(data, 0x84, 0x7f, 0xff, 0xff, 0xff)
	data = append(data, 0, 0, 0, 0)
	f, err := ParseMXF("bad.mxf", data, DefaultLimits())
	fe, ok := fault.As(err)
	if !ok {
		t.Fatalf("expected fault, got %v", err)
	}
	if fe.Kind != fault.KindLimit && fe.Kind != fault.KindTruncated {
		t.Fatalf("kind = %s", fe.Kind)
	}
	if fe.Offset < 0 {
		t.Fatal("offset must be reported for oversized KLV")
	}
	// The fault type must be visible through errors.Is.
	if !strings.Contains(f.Faults.String(), "KLV") {
		t.Fatalf("fault list should mention KLV: %v", f.Faults)
	}
}

func TestLimitsNormalize(t *testing.T) {
	if _, err := (Limits{MaxTracks: -1}).Normalize(); err == nil {
		t.Fatal("negative limit must be rejected")
	}
	l, err := Limits{}.Normalize()
	if err != nil {
		t.Fatalf("normalize zero: %v", err)
	}
	if l.MaxFileSize <= 0 || l.MaxTracks <= 0 || l.MaxKLV <= 0 {
		t.Fatalf("zero limits not defaulted: %+v", l)
	}
}

// resolveTrack applies metadata resolution to one track.
func resolveTrack(t *model.Track) {
	metadata.ResolveAll([]*model.Track{t}, metadata.Options{})
}

// findNestedBox searches the whole file for a box type, returning the offset of
// its header. Box types are four printable (or 0xa9) bytes so a byte scan is
// unambiguous enough for test fixtures.
func findNestedBox(t *testing.T, data []byte, typ string) int {
	t.Helper()
	needle := []byte(typ)
	for i := 4; i+4 <= len(data); i++ {
		if string(data[i:i+4]) != string(needle) {
			continue
		}
		size := int(binary.BigEndian.Uint32(data[i-4 : i]))
		if size >= 8 && i-4+size <= len(data) {
			return i - 4
		}
	}
	t.Fatalf("box %q not found", typ)
	return -1
}

// findBox returns the offset of the first top-level box of the given type.
func findBox(t *testing.T, data []byte, typ string) int {
	t.Helper()
	pos := 0
	for pos+8 <= len(data) {
		size := int(binary.BigEndian.Uint32(data[pos : pos+4]))
		if size < 8 {
			break
		}
		if string(data[pos+4:pos+8]) == typ {
			return pos
		}
		pos += size
	}
	t.Fatalf("box %q not found", typ)
	return -1
}
