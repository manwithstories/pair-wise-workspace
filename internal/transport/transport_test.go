package transport

import (
	"math/rand"
	"testing"

	"github.com/pion/rtp"
)

func TestPackagerSequenceAndTimestamp(t *testing.T) {
	p := NewPackager(DefaultPacketConfig())
	payload := make([]byte, 2500)
	envs, err := p.Pack(SentSample{TrackID: 1, Index: 0, MediaUs: 0, DurationUs: 33_333, Payload: payload})
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if len(envs) != 3 {
		t.Fatalf("fragments = %d", len(envs))
	}
	for i, e := range envs {
		if e.Packet.SequenceNumber != uint16(i) {
			t.Fatalf("seq %d = %d", i, e.Packet.SequenceNumber)
		}
		if e.Packet.Timestamp != 0 {
			t.Fatalf("first sample ts = %d", e.Packet.Timestamp)
		}
		wantMarker := i == 2
		if e.Packet.Marker != wantMarker {
			t.Fatalf("frag %d marker = %v", i, e.Packet.Marker)
		}
	}
	envs2, _ := p.Pack(SentSample{TrackID: 1, Index: 1, MediaUs: 33_333, DurationUs: 33_333, Payload: make([]byte, 10)})
	if envs2[0].Packet.Timestamp != 2999 {
		t.Fatalf("second sample ts = %d", envs2[0].Packet.Timestamp)
	}
	if p.PacketsSent() != 4 {
		t.Fatalf("sent = %d", p.PacketsSent())
	}
}

func TestPackagerRejectsEmpty(t *testing.T) {
	p := NewPackager(DefaultPacketConfig())
	if _, err := p.Pack(SentSample{TrackID: 1, Index: 0}); err == nil {
		t.Fatal("expected error for empty payload")
	}
	if _, err := p.Pack(SentSample{Index: 0, Payload: []byte{1}}); err == nil {
		t.Fatal("expected error for missing track")
	}
}

func TestPackagerRoundTripMarshal(t *testing.T) {
	p := NewPackager(DefaultPacketConfig())
	envs, _ := p.Pack(SentSample{TrackID: 1, Index: 0, Payload: make([]byte, 1400)})
	for _, e := range envs {
		raw, err := e.Packet.Marshal()
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var decoded rtp.Packet
		if err := decoded.Unmarshal(raw); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if decoded.SequenceNumber != e.Packet.SequenceNumber || decoded.Timestamp != e.Packet.Timestamp {
			t.Fatalf("round trip mismatch")
		}
	}
}

func TestReorderWindowBounded(t *testing.T) {
	w := NewReorderWindow(4)
	mk := func(seq uint16) *rtp.Packet { return &rtp.Packet{Header: rtp.Header{SequenceNumber: seq}} }
	// Arrive 0, then 3,2,1 within the horizon.
	if ready, ok := w.Push(mk(0)); !ok || len(ready) != 1 {
		t.Fatalf("push 0: %v %v", ready, ok)
	}
	for _, s := range []uint16{3, 2, 1} {
		if _, ok := w.Push(mk(s)); !ok {
			t.Fatalf("push %d rejected", s)
		}
	}
	if w.Pending() != 0 {
		t.Fatalf("pending = %d", w.Pending())
	}
	// A far-future packet forces the head forward; gaps are skipped and the
	// far packet stays buffered until the head reaches it.
	if _, ok := w.Push(mk(100)); !ok {
		t.Fatal("far packet rejected")
	}
	if w.HorizonJumps() == 0 || len(w.Skipped()) == 0 {
		t.Fatalf("expected horizon jump and skipped positions: jumps=%d skipped=%v", w.HorizonJumps(), w.Skipped())
	}
	flushed := w.Flush()
	if len(flushed) != 1 || flushed[0].SequenceNumber != 100 {
		t.Fatalf("flush = %+v", flushed)
	}
}

func TestReorderWindowTooOld(t *testing.T) {
	w := NewReorderWindow(8)
	w.Push(&rtp.Packet{Header: rtp.Header{SequenceNumber: 10}})
	if _, ok := w.Push(&rtp.Packet{Header: rtp.Header{SequenceNumber: 9}}); ok {
		t.Fatal("old packet should be rejected")
	}
	if w.TooOld() != 1 {
		t.Fatalf("tooOld = %d", w.TooOld())
	}
}

func TestReceiverCleanStream(t *testing.T) {
	p := NewPackager(DefaultPacketConfig())
	recv := NewReceiver(32, 90000)
	sent := 0
	for i := 0; i < 10; i++ {
		envs, _ := p.Pack(SentSample{TrackID: 1, Index: i, MediaUs: int64(i) * 33_333, DurationUs: 33_333, Payload: make([]byte, 300)})
		for _, e := range envs {
			recv.Push(e.Packet, e.MediaUs)
			sent++
		}
	}
	st := recv.Finish(0, sent)
	if st.Delivered != sent || st.Lost != 0 || st.RecoveryRate != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestReceiverLossAndRecoveryDeterministic(t *testing.T) {
	envs := makeEnv(t, 200)
	sim := Simulation{LossFraction: 0.15, ReorderFraction: 0.4, MaxReorder: 4, DuplicateFraction: 0.1}
	a := Simulate(envs, 32, 90000, sim, rand.New(rand.NewSource(7)))
	b := Simulate(envs, 32, 90000, sim, rand.New(rand.NewSource(7)))
	if a.Stats.Lost != b.Stats.Lost || a.Stats.Delivered != b.Stats.Delivered {
		t.Fatalf("determinism: %+v vs %+v", a.Stats, b.Stats)
	}
	if a.Stats.PacketsSent != 200 {
		t.Fatalf("sent = %d", a.Stats.PacketsSent)
	}
	if a.Stats.Reordered == 0 && sim.ReorderFraction > 0 {
		t.Fatalf("expected reordering, stats = %+v", a.Stats)
	}
	if a.Stats.Duplicates == 0 {
		t.Fatalf("expected duplicates, stats = %+v", a.Stats)
	}
	if a.Stats.Lost == 0 {
		t.Fatalf("expected loss, stats = %+v", a.Stats)
	}
	if a.Stats.RecoveryRate >= 1 {
		t.Fatalf("recovery rate = %v", a.Stats.RecoveryRate)
	}
}

func TestMissingWindows(t *testing.T) {
	envs := []Envelope{
		{Packet: &rtp.Packet{Header: rtp.Header{SequenceNumber: 1}}, MediaUs: 0, DurationUs: 33_000},
		{Packet: &rtp.Packet{Header: rtp.Header{SequenceNumber: 2}}, MediaUs: 33_000, DurationUs: 33_000},
		{Packet: &rtp.Packet{Header: rtp.Header{SequenceNumber: 3}}, MediaUs: 66_000, DurationUs: 33_000},
	}
	delivered := map[uint16]bool{1: true, 3: true}
	cov := MissingWindows(envs, delivered, 33_000)
	if len(cov) != 1 || cov[0].StartUs != 33_000 || cov[0].EndUs != 66_000 {
		t.Fatalf("coverage = %+v", cov)
	}
}

func TestOutOfWindowCounted(t *testing.T) {
	envs := makeEnv(t, 60)
	sim := Simulation{OutOfWindowFraction: 0.25}
	res := Simulate(envs, 8, 90000, sim, rand.New(rand.NewSource(1)))
	if res.Stats.Unrecoverable == 0 {
		t.Fatalf("expected unrecoverable packets: %+v", res.Stats)
	}
	if len(res.OutOfWindowSeq) == 0 {
		t.Fatal("expected out-of-window sequences")
	}
}

func makeEnv(t *testing.T, n int) []Envelope {
	t.Helper()
	p := NewPackager(DefaultPacketConfig())
	var envs []Envelope
	for i := 0; i < n; i++ {
		got, err := p.Pack(SentSample{TrackID: 1, Index: i, MediaUs: int64(i) * 33_333, DurationUs: 33_333, Payload: make([]byte, 64)})
		if err != nil {
			t.Fatal(err)
		}
		envs = append(envs, got...)
	}
	return envs
}
