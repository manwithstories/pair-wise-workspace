package merge

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"medpipe/internal/fault"
	rtpstream "medpipe/internal/rtp"

	"github.com/pion/rtp"
)

func makeRec(track uint32, seq uint16, ts uint32, payload []byte) *rtpstream.Record {
	pkt := &rtp.Packet{
		Header: rtp.Header{Version: 2, PayloadType: 96,
			SequenceNumber: seq, Timestamp: ts, SSRC: track*1000 + 7},
		Payload: payload,
	}
	return &rtpstream.Record{Type: rtpstream.RecRTP, Track: track, Packet: pkt}
}

func decode(_ context.Context, j Job) Result {
	h := sha256.Sum256(j.Rec.Packet.Payload)
	return Result{Ev: &Event{
		Track:        j.Rec.Track,
		Seq:          j.Rec.Packet.SequenceNumber,
		RTPTimestamp: j.Rec.Packet.Timestamp,
		Payload:      j.Rec.Packet.Payload,
		Hash:         h,
		Offset:       j.Rec.Offset,
	}}
}

func TestPoolLimits(t *testing.T) {
	if _, err := NewPool(context.Background(), 0, 8, decode); err == nil {
		t.Fatal("expected worker limit error")
	}
	if _, err := NewPool(context.Background(), 2, 0, decode); err == nil {
		t.Fatal("expected queue limit error")
	}
	if _, err := NewPool(context.Background(), MaxWorkers+1, 8, decode); err == nil {
		t.Fatal("expected worker hard-cap error")
	}
}

func TestPoolBackpressureAndCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// No consumer: the queue (capacity 2) fills and Submit blocks.
	p, err := NewPool(ctx, 1, 2, func(_ context.Context, j Job) Result {
		time.Sleep(300 * time.Millisecond) // slow decode
		return decode(ctx, j)
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := makeRec(0, 1, 0, []byte("x"))
	_ = p.Submit(ctx, Job{Rec: rec})
	_ = p.Submit(ctx, Job{Rec: rec})
	_ = p.Submit(ctx, Job{Rec: rec})
	done := make(chan error, 1)
	go func() { done <- p.Submit(ctx, Job{Rec: rec}) }()
	select {
	case <-done:
		t.Fatal("submit should block on a full queue")
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("submit should fail after cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("submit did not unblock after cancel")
	}
	p.Close()
}

func TestPoolResultsAndClose(t *testing.T) {
	ctx := context.Background()
	p, err := NewPool(ctx, 4, 16, decode)
	if err != nil {
		t.Fatal(err)
	}
	const n = 50
	// The consumer must run concurrently: with a bounded results
	// channel, submitting everything before draining would deadlock.
	got := 0
	done := make(chan struct{})
	go func() {
		for range p.Results() {
			got++
		}
		close(done)
	}()
	for i := 0; i < n; i++ {
		if err := p.Submit(ctx, Job{Rec: makeRec(0, uint16(i), uint32(i)*960, []byte{byte(i)})}); err != nil {
			t.Fatal(err)
		}
	}
	p.Close()
	<-done
	if got != n {
		t.Fatalf("got %d results, want %d", got, n)
	}
}

func TestReorderDedupAndGaps(t *testing.T) {
	fl := &fault.List{}
	b := newTrackBuffer(0, 4)
	push := func(seq uint16, tag byte) []*Event {
		ev := &Event{Track: 0, Seq: seq, Payload: []byte{tag},
			Hash: sha256.Sum256([]byte{tag})}
		return b.push(ev, fl)
	}
	// Nothing emits before the window is primed.
	if out := push(0, 'a'); len(out) != 0 {
		t.Fatalf("emit before priming: %d", len(out))
	}
	// Out-of-order arrival inside the window is buffered.
	if out := push(2, 'c'); len(out) != 0 {
		t.Fatalf("early emit: %d", len(out))
	}
	// Duplicate of a buffered sequence.
	if out := push(0, 'a'); len(out) != 0 || b.Dups != 1 {
		t.Fatalf("dup not detected: %d dups=%d", len(out), b.Dups)
	}
	// Gap at 1: pushing 5 (window 4 from next=0) declares 1 missing,
	// primes the buffer and releases 0 and 2 in order.
	out := push(5, 'f')
	if len(out) != 2 || out[0].Seq != 0 || out[1].Seq != 2 ||
		out[1].MissingBefore != 1 {
		t.Fatalf("gap handling: %+v", out)
	}
	if fl.Count(fault.KindLost) == 0 {
		t.Fatal("expected located loss faults")
	}
	// Too-late packet (already declared missing).
	if out := push(1, 'b'); len(out) != 0 || b.TooLate != 1 {
		t.Fatalf("late packet: %d late=%d", len(out), b.TooLate)
	}
}

func TestReorderSequenceWrap(t *testing.T) {
	fl := &fault.List{}
	b := newTrackBuffer(0, 8)
	var order []uint64
	push := func(seq uint16) {
		ev := &Event{Track: 0, Seq: seq, Payload: []byte{byte(seq)},
			Hash: sha256.Sum256([]byte{byte(seq)})}
		for _, e := range b.push(ev, fl) {
			order = append(order, e.ExtSeq)
		}
	}
	// Stream starting just before the 16-bit wrap.
	for _, s := range []uint16{65534, 65535, 0, 1, 2, 3} {
		push(s)
	}
	for _, e := range b.flush() {
		order = append(order, e.ExtSeq)
	}
	want := []uint64{65534, 65535, 65536, 65537, 65538, 65539}
	if len(order) != len(want) {
		t.Fatalf("order = %v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestMergerDeterministicOrder(t *testing.T) {
	run := func() []Event {
		fl := &fault.List{}
		m := NewMerger(16, 0.5, fl)
		// Two tracks, pairwise-swapped arrival order, shared clock domain.
		order := []int{1, 0, 3, 2, 5, 4, 7, 6, 9, 8}
		for _, i := range order {
			for tr := uint32(0); tr < 2; tr++ {
				ts := uint32(i) * 960
				m.AddEvent(&Event{Track: tr, Seq: uint16(i), RTPTimestamp: ts,
					Payload: []byte{byte(i), byte(tr)},
					Hash:    sha256.Sum256([]byte{byte(i), byte(tr)})})
			}
		}
		// Calibrate both tracks identically: shared = ts/48000.
		for tr := uint32(0); tr < 2; tr++ {
			m.AddSenderReport(tr, &rtpstream.SenderReport{
				SSRC: tr*1000 + 7, RTPTimestamp: 0, PacketCount: 10}, 0)
		}
		m.Flush()
		var out []Event
		for {
			ev := m.Pop()
			if ev == nil {
				break
			}
			out = append(out, *ev)
		}
		return out
	}
	a, b := run(), run()
	if len(a) != 20 || len(b) != 20 {
		t.Fatalf("merged %d/%d events", len(a), len(b))
	}
	for i := range a {
		if a[i].Shared != b[i].Shared || a[i].Track != b[i].Track ||
			a[i].ExtSeq != b[i].ExtSeq {
			t.Fatalf("nondeterministic merge at %d", i)
		}
		if i > 0 {
			prev, cur := a[i-1], a[i]
			if prev.Shared.Cmp(cur.Shared) > 0 {
				t.Fatalf("timeline not ordered at %d", i)
			}
		}
	}
}

func TestMergerDriftIsolation(t *testing.T) {
	fl := &fault.List{}
	m := NewMerger(16, 0.5, fl)
	// Track 0 gets a stable calibration; then its clock jumps 2s.
	m.AddSenderReport(0, &rtpstream.SenderReport{SSRC: 7, NTPSec: 1000,
		RTPTimestamp: 0, PacketCount: 5}, 0)
	m.AddSenderReport(0, &rtpstream.SenderReport{SSRC: 7, NTPSec: 1002,
		RTPTimestamp: 4800, PacketCount: 10}, 64)
	if len(m.Anomalies) != 1 {
		t.Fatalf("anomalies = %d, want 1", len(m.Anomalies))
	}
	if fl.Count(fault.KindDrift) == 0 {
		t.Fatal("drift fault not recorded")
	}
	// Packets of the isolated track are dropped; other tracks unaffected.
	m.AddEvent(&Event{Track: 0, Seq: 1, RTPTimestamp: 960,
		Payload: []byte{1}, Hash: sha256.Sum256([]byte{1})})
	m.AddSenderReport(1, &rtpstream.SenderReport{SSRC: 1007, NTPSec: 1000,
		RTPTimestamp: 0, PacketCount: 5}, 0)
	m.AddEvent(&Event{Track: 1, Seq: 1, RTPTimestamp: 960,
		Payload: []byte{2}, Hash: sha256.Sum256([]byte{2})})
	m.Flush()
	var got []*Event
	for {
		ev := m.Pop()
		if ev == nil {
			break
		}
		got = append(got, ev)
	}
	if len(got) != 1 || got[0].Track != 1 {
		t.Fatalf("isolation leaked: %d events", len(got))
	}
	stats := m.Stats()
	var isolated, clean bool
	for _, s := range stats {
		if s.Track == 0 && s.Isolated {
			isolated = true
		}
		if s.Track == 1 && !s.Isolated && s.Received == 1 {
			clean = true
		}
	}
	if !isolated || !clean {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestMergerTailLossFromSenderReport(t *testing.T) {
	fl := &fault.List{}
	m := NewMerger(16, 0.5, fl)
	m.AddSenderReport(0, &rtpstream.SenderReport{SSRC: 7, NTPSec: 1000,
		RTPTimestamp: 0, PacketCount: 10}, 0)
	// Only 7 of 10 packets arrive; the last 3 are tail loss.
	for i := 0; i < 7; i++ {
		m.AddEvent(&Event{Track: 0, Seq: uint16(i), RTPTimestamp: uint32(i) * 960,
			Payload: []byte{byte(i)}, Hash: sha256.Sum256([]byte{byte(i)})})
	}
	m.Flush()
	for m.Pop() != nil {
	}
	stats := m.Stats()
	if len(stats) != 1 || stats[0].TailLoss != 3 || stats[0].Received != 7 {
		t.Fatalf("stats = %+v", stats)
	}
}
