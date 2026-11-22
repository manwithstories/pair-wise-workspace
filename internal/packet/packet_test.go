package packet

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pion/rtp"

	"mediaroute/internal/clock"
	"mediaroute/internal/manifest"
)

func makeRTP(t *testing.T, seq uint16, ts uint32) []byte {
	t.Helper()
	raw, err := (&rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts, SSRC: 41},
		Payload: bytes.Repeat([]byte{0xAB}, 100),
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestReadPacketRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(EncodeFrame(makeRTP(t, 1, 90000)))
	buf.Write(EncodeFrame(makeRTP(t, 2, 93000)))
	pkt, err := ReadPacket(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if pkt.Timestamp != 90000 || pkt.SequenceNumber != 1 {
		t.Fatalf("pkt %+v", pkt.Header)
	}
	pkt, err = ReadPacket(&buf)
	if err != nil || pkt.Timestamp != 93000 {
		t.Fatalf("pkt2 %v %+v", err, pkt)
	}
}

func TestReadPacketTruncated(t *testing.T) {
	frame := EncodeFrame(makeRTP(t, 1, 1))
	if _, err := ReadPacket(bytes.NewReader(frame[:len(frame)-10])); !errors.Is(err, ErrTruncated) {
		t.Fatalf("err %v want ErrTruncated", err)
	}
}

func TestReadPacketCorrupt(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(EncodeFrame([]byte{0xFF, 0x01, 0x02})) // not RTP
	if _, err := ReadPacket(&buf); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err %v want ErrCorrupt", err)
	}
}

func TestIndexBuilderAndAtomicWrite(t *testing.T) {
	b := NewBuilder()
	if err := b.AddKeyframe(0, 0, 100); err != nil {
		t.Fatal(err)
	}
	if err := b.AddKeyframe(2, 360000, 200); err != nil {
		t.Fatal(err)
	}
	if err := b.AddKeyframe(1, 180000, 150); err == nil {
		t.Fatal("out-of-order segment accepted")
	}
	path := filepath.Join(t.TempDir(), "index.jsonl")
	if err := b.WriteAtomic(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"segment":2`)) {
		t.Fatalf("index content %s", data)
	}
}

func TestVerify(t *testing.T) {
	intervals := []manifest.Interval{
		{Seq: 0, Start: 0, End: 180000},
		{Seq: 1, Start: 180000, End: 360000},
	}
	// Within one frame: passes.
	entries := []Entry{
		{Segment: 0, Ticks: 1000},
		{Segment: 1, Ticks: 180000 + clock.TicksPerFrame30},
	}
	mean, max, fails := Verify(entries, intervals, clock.TicksPerFrame30)
	if len(fails) != 0 {
		t.Fatalf("fails %v", fails)
	}
	if mean <= 0 || max != clock.TicksToMillisFloat(clock.TicksPerFrame30) {
		t.Fatalf("mean %v max %v", mean, max)
	}
	// Beyond one frame: reported.
	entries[1].Ticks += clock.TicksPerFrame30
	_, _, fails = Verify(entries, intervals, clock.TicksPerFrame30)
	if len(fails) != 1 {
		t.Fatalf("fails %v", fails)
	}
}
