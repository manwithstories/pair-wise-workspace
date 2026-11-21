package stream

import (
	"errors"
	"os"
	"testing"

	"github.com/pion/rtp"

	"mediascan/internal/fixture"
	"mediascan/internal/model"
)

func gen(t *testing.T) fixture.Layout {
	t.Helper()
	dir := t.TempDir()
	l, err := fixture.Generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestLoadInitTwoTracks(t *testing.T) {
	l := gen(t)
	init, err := LoadInit(l.VideoInit)
	if err != nil {
		t.Fatal(err)
	}
	if len(init.Tracks) != 2 {
		t.Fatalf("tracks=%d, want 2", len(init.Tracks))
	}
	v := init.Get(1)
	if v.Timescale != fixture.VideoTimescale || v.Kind != "video" {
		t.Fatalf("video meta: %+v", v)
	}
	a := init.Get(2)
	if a.Timescale != fixture.AudioTimescale || a.Kind != "audio" {
		t.Fatalf("audio meta: %+v", a)
	}
}

func TestScanSegmentClean(t *testing.T) {
	l := gen(t)
	init, err := LoadInit(l.VideoInit)
	if err != nil {
		t.Fatal(err)
	}
	path := fixture.SegmentPath(l.Work, 1)
	scan, err := ScanSegment(init, "seg-01.m4s", path, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Faults) != 0 {
		t.Fatalf("clean segment faults: %+v", scan.Faults)
	}
	if len(scan.Trafs) != 2 {
		t.Fatalf("traf count: %d", len(scan.Trafs))
	}
	if len(scan.Trafs[0].Samples) != fixture.SamplesPerVideoSegment ||
		len(scan.Trafs[1].Samples) != fixture.SamplesPerAudioSegment {
		t.Fatalf("sample counts: %d/%d", len(scan.Trafs[0].Samples), len(scan.Trafs[1].Samples))
	}
	if !scan.Trafs[0].Samples[0].Sync {
		t.Fatal("first sample of seg 1 should be a keyframe")
	}
	if scan.Trafs[0].BaseTime != fixture.VideoTimescale*fixture.SegmentSeconds {
		t.Fatalf("base time %d", scan.Trafs[0].BaseTime)
	}
}

func TestScanSegmentTruncatedBox(t *testing.T) {
	l := gen(t)
	init, err := LoadInit(l.VideoInit)
	if err != nil {
		t.Fatal(err)
	}
	path := fixture.SegmentPath(l.Work, 3)
	scan, err := ScanSegment(init, "seg-03.m4s", path, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	fault, ok := scan.FirstUndecodable()
	if !ok {
		t.Fatal("expected a structural fault")
	}
	if fault.Kind != model.FaultTruncatedBox {
		t.Fatalf("kind=%s: %s", fault.Kind, fault.Message)
	}
	info, _ := os.Stat(path)
	if fault.ByteEnd < fault.ByteStart || fault.ByteEnd >= info.Size() {
		t.Fatalf("fault range %d-%d outside file size %d", fault.ByteStart, fault.ByteEnd, info.Size())
	}
}

func TestScanSegmentImpossibleBoxSize(t *testing.T) {
	l := gen(t)
	init, err := LoadInit(l.VideoInit)
	if err != nil {
		t.Fatal(err)
	}
	path := fixture.SegmentPath(l.Work, 2)
	scan, err := ScanSegment(init, "seg-02.m4s", path, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	fault, ok := scan.FirstUndecodable()
	if !ok || fault.Kind != model.FaultBoxBounds {
		t.Fatalf("fault=%+v ok=%v", fault, ok)
	}
}

func TestScanSegmentByteRange(t *testing.T) {
	l := gen(t)
	init, err := LoadInit(l.VideoInit)
	if err != nil {
		t.Fatal(err)
	}
	path := fixture.SegmentPath(l.Work, 1)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mid := info.Size() / 2
	scan, err := ScanSegment(init, "seg-01.m4s", path, 0, mid)
	if err != nil {
		t.Fatal(err)
	}
	if scan.ByteEnd != mid {
		t.Fatalf("byte end %d want %d", scan.ByteEnd, mid)
	}
	if _, ok := scan.FirstUndecodable(); !ok {
		t.Fatal("a mid-file range cut should truncate the trailing box")
	}
	if _, err := ScanSegment(init, "seg-01.m4s", path, 10, 5); !errors.Is(err, model.ErrBoxBounds) {
		t.Fatalf("inverted range: %v", err)
	}
}

func TestReadCaptureAndAnalyze(t *testing.T) {
	l := gen(t)
	dropped, err := fixture.GenerateRTP(l.RTPFile, 60, 17)
	if err != nil {
		t.Fatal(err)
	}
	pkts, err := ReadCapture(l.RTPFile)
	if err != nil {
		t.Fatal(err)
	}
	stats := AnalyzeRTP(pkts)
	if stats.Packets != 60-dropped+1 { // +1 duplicate
		t.Fatalf("packets=%d", stats.Packets)
	}
	if stats.Dropped != dropped {
		t.Fatalf("dropped=%d want %d", stats.Dropped, dropped)
	}
	if stats.Duplicates < 1 {
		t.Fatalf("duplicates=%d", stats.Duplicates)
	}
	if stats.Reordered < 2 {
		t.Fatalf("reordered=%d", stats.Reordered)
	}
	if stats.RecoveryPct >= 100 || stats.RecoveryPct <= 0 {
		t.Fatalf("recovery pct %v", stats.RecoveryPct)
	}
	if stats.FirstLostSeq == 0 {
		t.Fatal("first lost sequence not reported")
	}
}

func TestAnalyzeRTPDuplicateNotRecovery(t *testing.T) {
	// Deliver every packet twice; a duplicate must not be counted as a
	// recovery of a gap.
	var pkts []*rtpPacket
	for i := 0; i < 10; i++ {
		pkts = append(pkts, mkPkt(uint16(i), uint32(i)*3000))
		pkts = append(pkts, mkPkt(uint16(i), uint32(i)*3000))
	}
	stats := AnalyzeRTP(convert(pkts))
	if stats.Dropped != 0 {
		t.Fatalf("dropped=%d", stats.Dropped)
	}
	if stats.Duplicates != 10 {
		t.Fatalf("duplicates=%d", stats.Duplicates)
	}
	if stats.RecoveryPct != 100 {
		t.Fatalf("recovery=%v", stats.RecoveryPct)
	}
}

func TestAnalyzeRTPWindowBoundary(t *testing.T) {
	// Inside the window: 3 and 4 arrive after 5 and are reordered.
	pkts := convert([]*rtpPacket{
		mkPkt(0, 0), mkPkt(1, 3000), mkPkt(2, 6000),
		mkPkt(5, 15000), mkPkt(3, 9000), mkPkt(4, 12000),
	})
	stats := AnalyzeRTP(pkts)
	if stats.Reordered != 2 {
		t.Fatalf("reordered=%d", stats.Reordered)
	}
	if stats.Dropped != 0 {
		t.Fatalf("dropped=%d", stats.Dropped)
	}

	// Beyond the window: 3 arrives 97 behind 100 and cannot be recovered.
	pkts = convert([]*rtpPacket{
		mkPkt(0, 0), mkPkt(1, 3000), mkPkt(2, 6000), mkPkt(100, 300000), mkPkt(3, 9000),
	})
	stats = AnalyzeRTP(pkts)
	if stats.Reordered != 0 {
		t.Fatalf("out-of-window packet counted as reordered: %d", stats.Reordered)
	}
	if stats.Dropped == 0 || stats.FirstLostSeq == 0 {
		t.Fatalf("expected unrecovered loss: %+v", stats)
	}
}

// Helpers to build RTP packets without going through a capture file.
type rtpPacket struct {
	seq uint16
	ts  uint32
}

func mkPkt(seq uint16, ts uint32) *rtpPacket { return &rtpPacket{seq: seq, ts: ts} }

func convert(in []*rtpPacket) []*rtp.Packet {
	out := make([]*rtp.Packet, len(in))
	for i, p := range in {
		out[i] = &rtp.Packet{Header: rtp.Header{SequenceNumber: p.seq, Timestamp: p.ts}}
	}
	return out
}
