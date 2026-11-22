package rtppacket

import (
	"math"
	"testing"

	"github.com/pion/rtp"
)

func TestTimestampUses48kClock(t *testing.T) {
	if got := Timestamp(90000); got != 48000 {
		t.Fatalf("90k tick = %d", got)
	}
	if got := Timestamp(0); got != 0 {
		t.Fatalf("zero = %d", got)
	}
	if Timestamp(-5) != 0 {
		t.Fatal("negative ticks must clamp to zero")
	}
}

func TestBuildAndParseEvidence(t *testing.T) {
	ev, raw, err := Build("seg001.m4s", 7, 180000, 48000, -23)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Timestamp != 96000 || ev.Frames != 48000 {
		t.Fatalf("evidence = %+v", ev)
	}
	if ev.AudioLevel != 23 {
		t.Fatalf("level = %d", ev.AudioLevel)
	}
	ts, level, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if ts != ev.Timestamp || level != ev.AudioLevel {
		t.Fatalf("parsed = %d/%d", ts, level)
	}
	// Same inputs must produce identical bytes.
	_, raw2, err := Build("seg001.m4s", 7, 180000, 48000, -23)
	if err != nil || string(raw) != string(raw2) {
		t.Fatal("evidence is not deterministic")
	}
}

func TestLevelFromLUFS(t *testing.T) {
	cases := map[float64]uint8{-23: 23, -1: 1, 0: 0, 5: 0, -200: 127}
	for in, want := range cases {
		if got := LevelFromLUFS(in); got != want {
			t.Fatalf("level(%v) = %d want %d", in, got, want)
		}
	}
	if got := LevelFromLUFS(math.Inf(-1)); got != 127 {
		t.Fatalf("silence level = %d", got)
	}
	if got := LevelFromLUFS(math.NaN()); got != 127 {
		t.Fatalf("NaN level = %d", got)
	}
}

func TestBuildValidation(t *testing.T) {
	if _, _, err := Build("", 1, 0, 0, 0); err == nil {
		t.Fatal("empty uri accepted")
	}
	if _, _, err := Build("a", 1, 0, -1, 0); err == nil {
		t.Fatal("negative frames accepted")
	}
}

func TestMonotonic(t *testing.T) {
	if !Monotonic([]Evidence{{Timestamp: 1}, {Timestamp: 2}, {Timestamp: 3}}) {
		t.Fatal("increasing series rejected")
	}
	if Monotonic([]Evidence{{Timestamp: 1}, {Timestamp: 1}}) {
		t.Fatal("duplicate timestamps accepted")
	}
	if !Monotonic(nil) {
		t.Fatal("empty series must be monotonic")
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	if _, _, err := Parse([]byte{0, 1, 2}); err == nil {
		t.Fatal("short packet accepted")
	}
	p := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: 1, Timestamp: 10, SSRC: SSRC}}
	raw, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Parse(raw); err == nil {
		t.Fatal("packet without extension accepted")
	}
}
