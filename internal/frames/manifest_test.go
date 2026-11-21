package frames

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/pion/rtp"

	"mediapack/internal/fault"
)

func packet(t *testing.T, seq uint16, ts uint32) []byte {
	t.Helper()
	pkt := &rtp.Packet{Header: rtp.Header{
		Version:        2,
		PayloadType:    96,
		SequenceNumber: seq,
		Timestamp:      ts,
		SSRC:           0x1234,
	}, Payload: []byte{0xde, 0xad}}
	raw, err := pkt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func record(t *testing.T, n int, ts uint64, key bool, payload []byte) Record {
	t.Helper()
	rtpTS := uint32(ts) // tests use a 90000 timescale so units already match
	return Record{
		Number:    n,
		Timestamp: ts,
		Keyframe:  key,
		SHA256:    Sum(payload),
		RTPPacket: packet(t, uint16(n-1), rtpTS),
	}
}

func validManifest(t *testing.T) (*Manifest, map[int][]byte) {
	t.Helper()
	payloads := map[int][]byte{
		1: []byte("frame-one"),
		2: []byte("frame-two"),
		3: []byte("frame-three"),
	}
	recs := []Record{
		record(t, 1, 0, true, payloads[1]),
		record(t, 2, 3000, false, payloads[2]),
		record(t, 3, 6000, false, payloads[3]),
	}
	return &Manifest{Track: "v-main", Timescale: 90000, Records: recs}, payloads
}

func TestValidateAcceptsWellFormedManifest(t *testing.T) {
	m, payloads := validManifest(t)
	got, err := Validate(m, payloads, DefaultLimits())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("frames=%d want 3", len(got))
	}
	if got[1].Ticks.String() != "1/30" {
		t.Fatalf("frame 2 ticks=%s want 1/30", got[1].Ticks)
	}
	if !got[0].Keyframe || got[1].Keyframe {
		t.Fatalf("keyframe flags wrong")
	}
	if got[2].RTPSeq != 2 || got[2].RTPTS != 6000 {
		t.Fatalf("rtp fields wrong: seq=%d ts=%d", got[2].RTPSeq, got[2].RTPTS)
	}
}

func TestDuplicateSequenceReportsFirstFrame(t *testing.T) {
	m, payloads := validManifest(t)
	m.Records = append(m.Records, record(t, 2, 9000, false, payloads[2]))
	_, err := Validate(m, payloads, DefaultLimits())
	fe, ok := fault.As(err)
	if !ok || fe.Code != "duplicate-sequence" || fe.Frame != 2 {
		t.Fatalf("got %v want duplicate-sequence at frame 2", err)
	}
}

func TestMissingFrameReportsEarliestGap(t *testing.T) {
	m, payloads := validManifest(t)
	m.Records = append(m.Records, record(t, 5, 12000, false, []byte("five")))
	payloads[5] = []byte("five")
	_, err := Validate(m, payloads, DefaultLimits())
	fe, ok := fault.As(err)
	if !ok || fe.Code != "missing-frame" || fe.Frame != 4 {
		t.Fatalf("got %v want missing-frame at 4", err)
	}
}

func TestCorruptChecksumReportsBadFrame(t *testing.T) {
	m, payloads := validManifest(t)
	payloads[2] = []byte("frame-two-corrupted")
	_, err := Validate(m, payloads, DefaultLimits())
	fe, ok := fault.As(err)
	if !ok || fe.Code != "checksum-mismatch" || fe.Frame != 2 {
		t.Fatalf("got %v want checksum-mismatch at 2", err)
	}
}

func TestEarliestFaultWinsAcrossKinds(t *testing.T) {
	m, payloads := validManifest(t)
	m.Records[2].SHA256 = Sum([]byte("something else")) // frame 3 corrupt
	payloads[2] = []byte("changed")                     // frame 2 corrupt
	_, err := Validate(m, payloads, DefaultLimits())
	fe, ok := fault.As(err)
	if !ok || fe.Frame != 2 {
		t.Fatalf("got %v want earliest frame 2", err)
	}
}

func TestFrameLimitRejectsInput(t *testing.T) {
	m, payloads := validManifest(t)
	_, err := Validate(m, payloads, Limits{MaxFrameBytes: 1 << 20, MaxFrames: 2})
	if fault.ClassOf(err) != fault.ClassLimit {
		t.Fatalf("got %v want limit class", err)
	}
	if fault.ExitCode(err) != 2 {
		t.Fatalf("limit exit code = %d want 2", fault.ExitCode(err))
	}
}

func TestFrameByteLimitRejectsInput(t *testing.T) {
	m, payloads := validManifest(t)
	_, err := Validate(m, payloads, Limits{MaxFrameBytes: 4, MaxFrames: 100})
	if fault.ClassOf(err) != fault.ClassLimit || fault.ExitCode(err) != 2 {
		t.Fatalf("got %v", err)
	}
}

func TestRTPTimestampMismatchIsReported(t *testing.T) {
	m, payloads := validManifest(t)
	m.Records[1].RTPPacket = packet(t, 1, 12345)
	_, err := Validate(m, payloads, DefaultLimits())
	fe, ok := fault.As(err)
	if !ok || fe.Code != "rtp-time-mismatch" || fe.Frame != 2 {
		t.Fatalf("got %v", err)
	}
}

func TestInexactTimescaleConversionRejected(t *testing.T) {
	payload := []byte("x")
	m := &Manifest{Track: "v", Timescale: 7, Records: []Record{{
		Number: 1, Timestamp: 1, SHA256: Sum(payload), RTPPacket: packet(t, 0, 12857),
	}}}
	_, err := Validate(m, map[int][]byte{1: payload}, DefaultLimits())
	fe, ok := fault.As(err)
	if !ok || fe.Code != "unrepresentable-time" {
		t.Fatalf("got %v want unrepresentable-time", err)
	}
}

func TestParseRejectsUnknownFieldsAndBadTimescale(t *testing.T) {
	if _, err := Parse([]byte(`{"track":"v","timescale":90000,"frames":[],"extra":1}`), DefaultLimits()); fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("unknown field: %v", err)
	}
	if _, err := Parse([]byte(`{"track":"v","timescale":0,"frames":[]}`), DefaultLimits()); fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("zero timescale: %v", err)
	}
	if _, err := Parse([]byte(`not json`), DefaultLimits()); fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("bad json: %v", err)
	}
}

func TestParseRoundTrip(t *testing.T) {
	m, _ := validManifest(t)
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(raw, DefaultLimits())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(back.Records) != len(m.Records) || back.Track != m.Track {
		t.Fatalf("round trip mismatch")
	}
}

func TestValidSHA256(t *testing.T) {
	good := Sum([]byte("abc"))
	if !ValidSHA256(good) {
		t.Fatalf("computed digest rejected")
	}
	for _, bad := range []string{"", "abc", good[:63], "A" + good[1:], good + "00"} {
		if ValidSHA256(bad) {
			t.Fatalf("accepted invalid digest %q", bad)
		}
	}
}

func TestEarliestReturnsPlainError(t *testing.T) {
	plain := errors.New("plain")
	if earliest([]error{plain}) != plain {
		t.Fatalf("plain error was replaced")
	}
}
