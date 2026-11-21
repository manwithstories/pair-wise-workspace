package mp4

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	mp4 "github.com/abema/go-mp4"

	"mediapack/internal/fault"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	return Build(Options{Video: DefaultVideo(), Audio: DefaultAudio(),
		Mdat: []byte("sample-payload"), ExtraType: "wvnd", ExtraData: []byte{1, 2, 3}})
}

func TestCheckRoundTripsFixtureExactly(t *testing.T) {
	data := fixture(t)
	rt, err := Check(data, DefaultLimits())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !rt.ByteEqual || !rt.FieldsMatch {
		t.Fatalf("round trip failed: equal=%v fields=%v diffs=%v", rt.ByteEqual, rt.FieldsMatch, rt.Differences)
	}
	if rt.MatchPct != 100 {
		t.Fatalf("match_pct=%f", rt.MatchPct)
	}
	if rt.InputDigest != rt.OutputDigest || len(rt.InputDigest) != 64 {
		t.Fatalf("digests differ: %s vs %s", rt.InputDigest, rt.OutputDigest)
	}
	if rt.Unknown != 1 {
		t.Fatalf("unknown boxes=%d want 1", rt.Unknown)
	}
	if len(rt.Summary.Tracks) != 2 {
		t.Fatalf("tracks=%d", len(rt.Summary.Tracks))
	}
}

func TestSummaryFields(t *testing.T) {
	rt, err := Check(fixture(t), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	v := rt.Summary.Tracks[0]
	if v.Handler != "vide" || v.Width != 1920 || v.Height != 1080 {
		t.Fatalf("video summary wrong: %+v", v)
	}
	if v.SampleCount != 10 || len(v.SampleDeltas) != 10 || v.SampleDeltas[0] != 3000 {
		t.Fatalf("video timing wrong: %+v", v)
	}
	if len(v.Keyframes) != 2 || v.Keyframes[1] != 6 {
		t.Fatalf("keyframes=%v", v.Keyframes)
	}
	if v.Color == nil || v.Color.Primaries != 1 || v.Color.FullRange {
		t.Fatalf("color=%+v", v.Color)
	}
	a := rt.Summary.Tracks[1]
	if a.Handler != "soun" || a.Channels != 2 || a.SampleRate != 48000 {
		t.Fatalf("audio summary wrong: %+v", a)
	}
}

func TestUnknownBoxIsPreservedNotIgnored(t *testing.T) {
	data := Build(Options{Video: DefaultVideo(), ExtraType: "venc", ExtraData: []byte{9, 9, 9}})
	rt, err := Check(data, DefaultLimits())
	if err != nil {
		t.Fatalf("unknown box should parse: %v", err)
	}
	if rt.Unknown != 1 || !rt.ByteEqual {
		t.Fatalf("unknown box not preserved: unknown=%d equal=%v", rt.Unknown, rt.ByteEqual)
	}
	parsed, err := Parse(data, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	last := parsed.Nodes[len(parsed.Nodes)-1]
	if last.Type != mp4.StrToBoxType("venc") || !bytes.Equal(last.Raw, []byte{9, 9, 9}) {
		t.Fatalf("unknown payload lost: %+v", last)
	}
}

func TestTruncatedBoxRejected(t *testing.T) {
	data := fixture(t)
	// Chop the last four bytes: the final box now claims more than remains.
	truncated := data[:len(data)-4]
	_, err := Check(truncated, DefaultLimits())
	fe, ok := fault.As(err)
	if !ok || fe.Code != "truncated-box" {
		t.Fatalf("got %v want truncated-box", err)
	}
	if fault.ExitCode(err) != 2 {
		t.Fatalf("exit=%d", fault.ExitCode(err))
	}
}

func TestIllegalBoxSizeRejected(t *testing.T) {
	data := fixture(t)
	bad := append([]byte(nil), data...)
	binary.BigEndian.PutUint32(bad[0:], 4) // size below the 8-byte header
	_, err := Check(bad, DefaultLimits())
	fe, ok := fault.As(err)
	if !ok || fe.Code != "illegal-box-size" {
		t.Fatalf("got %v want illegal-box-size", err)
	}
}

func TestOversizedChildRejected(t *testing.T) {
	data := fixture(t)
	// Find the moov box and inflate its declared size beyond the file.
	bad := append([]byte(nil), data...)
	moovAt := bytes.Index(bad, []byte("moov")) - 4
	binary.BigEndian.PutUint32(bad[moovAt:], uint32(len(bad)))
	_, err := Check(bad, DefaultLimits())
	fe, ok := fault.As(err)
	if !ok || fe.Code != "truncated-box" || !strings.Contains(fe.Msg, "moov") {
		t.Fatalf("got %v", err)
	}
}

func TestMalformedPayloadRejected(t *testing.T) {
	data := fixture(t)
	// The stsz payload declares far more entries than the box holds.
	idx := bytes.Index(data, []byte("stsz"))
	if idx < 0 {
		t.Fatal("stsz not found")
	}
	bad := append([]byte(nil), data...)
	binary.BigEndian.PutUint32(bad[idx+12:], 1_000_000) // SampleCount
	_, err := Check(bad, DefaultLimits())
	if err == nil {
		t.Fatal("malformed stsz accepted")
	}
	if fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("class=%s err=%v", fault.ClassOf(err), err)
	}
}

func TestLimitsRejectOversizedInput(t *testing.T) {
	data := fixture(t)
	if _, err := Check(data, Limits{MaxBytes: 16, MaxTracks: 8, MaxBoxes: 100, MaxBoxDeep: 8}); fault.ClassOf(err) != fault.ClassLimit {
		t.Fatalf("got %v", err)
	}
	if _, err := Check(data, Limits{MaxBytes: 1 << 20, MaxTracks: 1, MaxBoxes: 100, MaxBoxDeep: 8}); fault.ClassOf(err) != fault.ClassLimit {
		t.Fatalf("track limit: %v", err)
	}
	if _, err := Check(data, Limits{MaxBytes: 1 << 20, MaxTracks: 8, MaxBoxes: 3, MaxBoxDeep: 8}); fault.ClassOf(err) != fault.ClassLimit {
		t.Fatalf("box limit: %v", err)
	}
}

func TestExtendsToEOFIsPreserved(t *testing.T) {
	data := Build(Options{Video: DefaultVideo(), Mdat: []byte("tail")})
	// Rewrite the mdat size field to zero, meaning "to end of file".
	idx := bytes.LastIndex(data, []byte("mdat")) - 4
	zeroed := append([]byte(nil), data...)
	binary.BigEndian.PutUint32(zeroed[idx:], 0)
	rt, err := Check(zeroed, DefaultLimits())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !rt.ByteEqual {
		t.Fatalf("size-0 box did not round trip byte-for-byte")
	}
}

func TestEmptyInputRejected(t *testing.T) {
	_, err := Check(nil, DefaultLimits())
	if fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("got %v", err)
	}
}
