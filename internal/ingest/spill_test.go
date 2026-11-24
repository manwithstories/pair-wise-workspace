package ingest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pion/rtp"

	"mediarun/internal/model"
	rtporder "mediarun/internal/rtp"
)

func TestSpillJournalLossless(t *testing.T) {
	dir := t.TempDir()
	s := mustStream(t, Config{TrackID: 11, Timescale: 1000, Window: 8, MaxBacklog: 2, SpillDir: dir})
	push := func(seq uint16) {
		if _, err := s.Push(rtporder.Packet{Sequence: seq, Timestamp: uint32(seq) * 1000, Payload: []byte{byte(seq)}}); err != nil {
			t.Fatalf("push %d: %v", seq, err)
		}
	}
	for i := uint16(0); i < 12; i++ {
		push(i)
	}
	if s.Stats().Spilled == 0 {
		t.Fatal("nothing spilled")
	}
	if _, err := os.Stat(filepath.Join(dir, "ingest-spill.jsonl")); err != nil {
		t.Fatalf("journal missing: %v", err)
	}
	var collected []model.Event
	for {
		evs, err := s.Drain(0)
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
		if len(evs) == 0 {
			break
		}
		collected = append(collected, evs...)
	}
	if len(collected) != 12 {
		t.Fatalf("lost events: got %d want 12", len(collected))
	}
	for i, ev := range collected {
		if ev.Sequence != uint32(i) {
			t.Fatalf("order broken at %d: sequence %d", i, ev.Sequence)
		}
	}
}

func TestSpillLimitReported(t *testing.T) {
	dir := t.TempDir()
	s := mustStream(t, Config{TrackID: 12, Timescale: 1000, Window: 8, MaxBacklog: 1, SpillDir: dir, SpillLimit: 64})
	var sawErr bool
	for i := uint16(0); i < 25; i++ {
		_, err := s.Push(rtporder.Packet{Sequence: i, Timestamp: uint32(i) * 10, Payload: make([]byte, 32)})
		if err != nil {
			if !errors.Is(err, model.ErrLimit) {
				t.Fatalf("unexpected error: %v", err)
			}
			sawErr = true
			break
		}
	}
	if !sawErr {
		t.Fatal("spill limit never reported")
	}
	// Nothing may be lost when the journal refuses the write.
	total := s.Pending() + s.Spilled()
	if total == 0 {
		t.Fatal("events vanished")
	}
}

func TestSpillRejectsWithoutDir(t *testing.T) {
	st := newSpillStore("", 1024)
	if _, err := st.Write([]model.Event{{Kind: model.KindFrame}}); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("no dir: %v", err)
	}
}

func TestSpillRemove(t *testing.T) {
	dir := t.TempDir()
	st := newSpillStore(dir, 1024)
	if _, err := st.Write([]model.Event{{Kind: model.KindFrame, Start: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ingest-spill.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("journal still present: %v", err)
	}
}

func TestReceivePionRoundTrip(t *testing.T) {
	s := mustStream(t, Config{TrackID: 13, Timescale: 90000})
	for i := uint16(0); i < 3; i++ {
		var p rtp.Packet
		p.SequenceNumber = i
		p.Timestamp = uint32(i) * 3000
		p.Payload = []byte{byte(i), 0xFF}
		wire, err := p.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Receive(wire); err != nil {
			t.Fatalf("receive %d: %v", i, err)
		}
	}
	evs, _ := s.Drain(0)
	if len(evs) != 3 {
		t.Fatalf("events %d", len(evs))
	}
	if evs[2].Start != 6000 {
		t.Fatalf("tick %d", evs[2].Start)
	}
}
