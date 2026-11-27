package media

import "testing"

func TestTimeConversions(t *testing.T) {
	if got := FromMillis(1250); got != TimeNS(1_250_000_000) {
		t.Fatalf("FromMillis = %d", got)
	}
	if got := FromMicros(7); got != TimeNS(7000) {
		t.Fatalf("FromMicros = %d", got)
	}
	if GraceWindow != 250*Millisecond {
		t.Fatalf("grace window = %d", GraceWindow)
	}
}

func TestRecordRoundTrip(t *testing.T) {
	f := &Frame{
		Version: WireVersion1, Codec: CodecPNG, CameraID: 7,
		Width: 640, Height: 480, Sequence: 42,
		EventNS: TimeNS(123456789), Payload: []byte("image-bytes"),
	}
	rec := EncodeRecord(f)
	if len(rec) != f.RecordLen() {
		t.Fatalf("record len = %d, want %d", len(rec), f.RecordLen())
	}
	g, err := ParseHeader(rec[:HeaderLen])
	if err != nil {
		t.Fatalf("parse header: %v", err)
	}
	if g.CameraID != 7 || g.Sequence != 42 || g.EventNS != f.EventNS ||
		g.Width != 640 || g.Height != 480 || g.Codec != CodecPNG {
		t.Fatalf("header mismatch: %+v", g)
	}
	if PayloadLen(rec[:HeaderLen]) != uint32(len(f.Payload)) {
		t.Fatalf("payload len mismatch")
	}
}

func TestParseHeaderRejectsBadVersionAndMagic(t *testing.T) {
	buf := make([]byte, HeaderLen)
	f := &Frame{Version: WireVersion1, Codec: CodecJPEG}
	PutHeader(buf, f, 0)

	bad := append([]byte(nil), buf...)
	bad[0] = 9
	if _, err := ParseHeader(bad); err == nil || AsFault(err).Stage != StageVersion {
		t.Fatalf("want version fault, got %v", err)
	}
	bad = append([]byte(nil), buf...)
	bad[36] = 'X'
	if _, err := ParseHeader(bad); err == nil || AsFault(err).Stage != StageHeader {
		t.Fatalf("want magic fault, got %v", err)
	}
}

func TestFaultCarriesCoordinates(t *testing.T) {
	err := NewFault(StageCRC, 1024, 3, 99, "crc mismatch")
	f := AsFault(err)
	if f == nil || f.Stage != StageCRC || f.ByteOffset != 1024 ||
		f.CameraID != 3 || f.Sequence != 99 || f.OffsetErr != 0 {
		t.Fatalf("fault coordinates lost: %+v", f)
	}
}
