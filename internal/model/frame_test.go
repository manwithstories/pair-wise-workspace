package model

import (
	"errors"
	"testing"
)

func TestTimebaseClock90(t *testing.T) {
	cases := []struct {
		tb    Timebase
		ticks uint64
		want  int64
	}{
		{Timebase{90000, 1}, 0, 0},
		{Timebase{90000, 1}, 1, 1},
		{Timebase{90000, 1}, 90, 90},
		// NTSC: 30000/1001 ticks/s -> one frame at tick 1 is 3003 ticks.
		{Timebase{30000, 1001}, 1, 3003},
		{Timebase{30000, 1001}, 30000, 90090000},
		// 90000/1 ticks per second at 1000 ticks/s.
		{Timebase{1000, 1}, 1, 90},
		{Timebase{1000, 1}, 1000, 90000},
	}
	for _, c := range cases {
		got, err := c.tb.Clock90(c.ticks)
		if err != nil {
			t.Fatalf("%v: %v", c.tb, err)
		}
		if got != c.want {
			t.Fatalf("%v tick %d: got %d want %d", c.tb, c.ticks, got, c.want)
		}
	}
}

func TestClock90Rounding(t *testing.T) {
	// 3 ticks per second (3/1), 7 s -> 630000 90 kHz ticks.
	if got, _ := (Timebase{3, 1}).Clock90(21); got != 630000 {
		t.Fatalf("got %d", got)
	}
	// 1 tick per 1.5 s (2/3): one tick -> 135000 90 kHz ticks.
	if got, _ := (Timebase{2, 3}).Clock90(1); got != 135000 {
		t.Fatalf("got %d", got)
	}
	// Half-up: 1 tick at 200000/1 = 0.45 -> 0; at 180001/1 = 0.49999 -> 0;
	// at 180000/1 = 0.5 -> 1; at 179999/1 = 0.5000028 -> 1.
	if got, _ := (Timebase{200000, 1}).Clock90(1); got != 0 {
		t.Fatalf("200000/1: got %d", got)
	}
	if got, _ := (Timebase{180001, 1}).Clock90(1); got != 0 {
		t.Fatalf("180001/1: got %d", got)
	}
	if got, _ := (Timebase{180000, 1}).Clock90(1); got != 1 {
		t.Fatalf("180000/1 half-up: got %d", got)
	}
	if got, _ := (Timebase{179999, 1}).Clock90(1); got != 1 {
		t.Fatalf("179999/1: got %d", got)
	}
}

func TestClock90Overflow(t *testing.T) {
	tb := Timebase{1, 1}
	if _, err := tb.Clock90(1 << 62); err == nil {
		t.Fatal("expected overflow error")
	}
	if _, err := tb.Clock90(1 << 20); err != nil {
		t.Fatal(err)
	}
}

func TestParseTimebase(t *testing.T) {
	tb, err := ParseTimebase("30000/1001")
	if err != nil || tb.Num != 30000 || tb.Den != 1001 {
		t.Fatalf("%+v %v", tb, err)
	}
	for _, bad := range []string{"", "30000", "0/1", "1/0", "a/b", "1/2/3", "-1/2"} {
		if _, err := ParseTimebase(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestRecordHeaderRoundTrip(t *testing.T) {
	h := RecordHeader{
		Version:   RecordVersion,
		Flags:     FlagMetadataPresent,
		Seq:       42,
		Timestamp: 123456789,
		Timebase:  Timebase{30000, 1001},
		MetaLen:   32,
		FrameLen:  1024,
	}
	for i := range h.Hash {
		h.Hash[i] = byte(i)
	}
	raw, err := h.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != RecordHeaderSize {
		t.Fatalf("header is %d bytes", len(raw))
	}
	got, err := ParseRecordHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got != h {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, h)
	}
}

func TestParseRecordHeaderRejects(t *testing.T) {
	good, _ := RecordHeader{Version: RecordVersion, Timebase: Timebase{1, 1}}.MarshalBinary()
	cases := map[string][]byte{
		"short":   good[:RecordHeaderSize-1],
		"magic":   append([]byte("XXXX"), good[4:]...),
		"version": append(append([]byte{}, good[:4]...), append([]byte{9}, good[5:]...)...),
	}
	for name, raw := range cases {
		if _, err := ParseRecordHeader(raw); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestFaultResync(t *testing.T) {
	if (&Fault{Code: CodeBadMagic}).Resync() {
		t.Fatal("bad magic must stop the scan")
	}
	if !(&Fault{Code: CodeHashMismatch}).Resync() {
		t.Fatal("hash mismatch is recoverable")
	}
	if !(&Fault{Code: CodeTruncatedPayload}).Resync() {
		t.Fatal("truncated body is reported at EOF")
	}
}

func TestFaultErrorString(t *testing.T) {
	f := &Fault{
		Code: CodeHashMismatch, Seq: 7, HasSeq: true, Offset: 1234,
		Expected: "aa", Actual: "bb", Reason: "payload changed",
	}
	s := f.Error()
	for _, want := range []string{"hash_mismatch", "frame=7", "offset=1234", "expected=aa", "actual=bb", "payload changed"} {
		if !contains(s, want) {
			t.Fatalf("%q missing %q", s, want)
		}
	}
	var target *Fault
	if !errors.As(f, &target) || target != f {
		t.Fatal("Fault must be its own unwrap target")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
