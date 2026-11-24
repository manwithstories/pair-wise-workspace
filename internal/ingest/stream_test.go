package ingest

import (
	"errors"
	"testing"

	"github.com/pion/rtp"

	"mediarun/internal/model"
	rtporder "mediarun/internal/rtp"
)

func mustStream(t *testing.T, cfg Config) *Stream {
	t.Helper()
	if cfg.Timescale == 0 {
		cfg.Timescale = 90000
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("new stream: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Config{TrackID: 0, Timescale: 90000}); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("track id: %v", err)
	}
	if _, err := New(Config{TrackID: 1, Timescale: 0}); err == nil {
		t.Fatal("timescale 0 accepted")
	}
}

func TestReceiveRejectsMalformed(t *testing.T) {
	s := mustStream(t, Config{TrackID: 1})
	if _, err := s.Receive([]byte{1, 2, 3}); !errors.Is(err, model.ErrTruncated) {
		t.Fatalf("short wire: %v", err)
	}
	if model.ClassOf(errors.New("x")) != model.ClassUnknown {
		t.Fatal("sanity")
	}
	var p rtp.Packet
	p.SequenceNumber = 1
	p.Timestamp = 90000
	p.Payload = []byte("x")
	wire, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	// Truncate the payload after the header: pion returns an error.
	if _, err := s.Receive(wire[:len(wire)-2]); err == nil {
		t.Fatal("truncated payload accepted")
	}
}

func TestReceiveOrdered(t *testing.T) {
	s := mustStream(t, Config{TrackID: 2, Timescale: 90000, Window: 8})
	var p rtp.Packet
	p.SequenceNumber = 1
	p.Timestamp = 9000
	p.Payload = []byte("a")
	p.Marker = true
	wire, _ := p.Marshal()
	rec, err := s.Receive(wire)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Duplicate || len(rec.Gaps) != 0 {
		t.Fatalf("receipt: %+v", rec)
	}
	evs, err := s.Drain(0)
	if err != nil || len(evs) != 1 {
		t.Fatalf("drain: %v %d", err, len(evs))
	}
	ev := evs[0]
	if ev.Kind != model.KindFrame || ev.Start != 0 || !ev.Keyframe {
		t.Fatalf("event: %+v", ev)
	}
	if string(ev.Payload) != "a" {
		t.Fatalf("payload: %q", ev.Payload)
	}
}

func TestReorderAndGap(t *testing.T) {
	s := mustStream(t, Config{TrackID: 3, Timescale: 1000, Window: 2, StartTick: 90000})
	push := func(seq uint16, ts uint32) Receipt {
		rec, err := s.Push(rtporder.Packet{Sequence: seq, Timestamp: ts, Payload: []byte{byte(seq)}})
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	push(0, 0)
	push(1, 1000)
	push(4, 4000) // 2,3 missing -> fills window? buffer has 1 entry
	rec := push(5, 5000)
	// Two packets were waiting in the window (4 and 5); the hole is 2..3.
	if len(rec.Gaps) != 1 || rec.Gaps[0].From != 2 || rec.Gaps[0].To != 3 {
		t.Fatalf("gap receipt: %+v", rec)
	}
	evs, _ := s.Drain(0)
	if len(evs) != 4 {
		t.Fatalf("events %d", len(evs))
	}
	// Buffered packets inside the hole are still delivered; only the
	// never-received sequences are reported as a gap.
	if evs[0].Start != 90000 || evs[1].Start != 180000 || evs[2].Start != 450000 || evs[3].Start != 540000 {
		t.Fatalf("ticks: %+v", evs)
	}
	st := s.Stats()
	if st.Gaps != 1 || st.GapPackets != 2 {
		t.Fatalf("stats %+v", st)
	}
}

func TestDuplicateCounted(t *testing.T) {
	s := mustStream(t, Config{TrackID: 3, Timescale: 1000, Window: 4})
	s.Push(rtporder.Packet{Sequence: 9, Timestamp: 0})
	rec, _ := s.Push(rtporder.Packet{Sequence: 9, Timestamp: 0})
	if !rec.Duplicate {
		t.Fatalf("duplicate: %+v", rec)
	}
	if st := s.Stats(); st.Duplicates != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestTimestampWrap(t *testing.T) {
	s := mustStream(t, Config{TrackID: 4, Timescale: 90000, Window: 8})
	s.Push(rtporder.Packet{Sequence: 0, Timestamp: 0xFFFFFF00})
	rec, _ := s.Push(rtporder.Packet{Sequence: 1, Timestamp: 0x00000100})
	if len(rec.Gaps) != 0 {
		t.Fatalf("unexpected gap: %+v", rec)
	}
	evs, _ := s.Drain(0)
	if len(evs) != 2 {
		t.Fatalf("events %d", len(evs))
	}
	if evs[1].Start != 0x200 {
		t.Fatalf("wrap tick %d", evs[1].Start)
	}
}

func TestBandwidthAdjustsBatchAndSpills(t *testing.T) {
	s := mustStream(t, Config{TrackID: 5, Timescale: 1000, Window: 8, MaxBacklog: 4, SpillDir: t.TempDir()})
	if s.Batch() != MinBatch {
		t.Fatalf("initial batch %d", s.Batch())
	}
	if err := s.SetBandwidth(8); err != nil {
		t.Fatal(err)
	}
	if s.Batch() != 64 {
		t.Fatalf("batch after 8mbps: %d", s.Batch())
	}
	if err := s.SetBandwidth(0); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("zero bandwidth: %v", err)
	}
	// Force a small cap then push more than it.
	if err := s.SetBandwidth(1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if _, err := s.Push(rtporder.Packet{Sequence: uint16(i), Timestamp: uint32(i * 10)}); err != nil {
			t.Fatal(err)
		}
	}
	st := s.Stats()
	if st.Spilled == 0 {
		t.Fatalf("expected spill, stats %+v", st)
	}
	if s.Spilled() == 0 {
		t.Fatal("spill store empty")
	}
	// Spilled events replay in order before the in-memory backlog.
	evs, err := s.Drain(0)
	if err != nil || len(evs) == 0 {
		t.Fatalf("drain after spill: %v %d", err, len(evs))
	}
	for i := 1; i < len(evs); i++ {
		if evs[i].Start < evs[i-1].Start {
			t.Fatalf("out of order replay at %d: %d < %d", i, evs[i].Start, evs[i-1].Start)
		}
	}
}

func TestCloseFlushesGap(t *testing.T) {
	s := mustStream(t, Config{TrackID: 6, Timescale: 1000, Window: 8})
	s.Push(rtporder.Packet{Sequence: 0, Timestamp: 0})
	s.Push(rtporder.Packet{Sequence: 2, Timestamp: 2000})
	rec, err := s.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Gaps) != 1 {
		t.Fatalf("close gaps: %+v", rec)
	}
	if _, err := s.Push(rtporder.Packet{Sequence: 3}); !errors.Is(err, ErrClosed) {
		t.Fatalf("push after close: %v", err)
	}
}

func TestForeignTrackRejected(t *testing.T) {
	s := mustStream(t, Config{TrackID: 7, Timescale: 1000})
	if _, err := s.Push(rtporder.Packet{TrackID: 8, Sequence: 0}); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("foreign track: %v", err)
	}
}

func TestBacklogCapGrowsWithBandwidth(t *testing.T) {
	s := mustStream(t, Config{TrackID: 9, Timescale: 1000, MaxBacklog: 64})
	low := s.BacklogCap()
	if err := s.SetBandwidth(10); err != nil {
		t.Fatal(err)
	}
	high := s.BacklogCap()
	if high <= low {
		t.Fatalf("cap did not grow: %d -> %d", low, high)
	}
	if high > 64*8 {
		t.Fatalf("cap exceeded hard bound: %d", high)
	}
}
