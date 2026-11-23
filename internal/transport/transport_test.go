package transport_test

import (
	"testing"

	"github.com/pion/rtp"

	"mediaroute/internal/fault"
	"mediaroute/internal/fixture"
	"mediaroute/internal/transport"
)

func TestReorderDedupAndWraparound(t *testing.T) {
	rb := transport.NewReorderBuffer(1, 1_000_000_000)
	// Force wraparound: start near the 16-bit ceiling.
	start := uint16(65534)
	seq := start
	push := func() uint16 {
		s := seq
		rb.Push(s, []byte{byte(s)}, 0)
		seq++
		return s
	}
	s0 := push() // 65534
	push()       // 65535
	s2 := push() // 0 after wrap
	s3 := push() // 1
	if s0 != 65534 || s2 != 0 || s3 != 1 {
		t.Fatalf("wrap sequence wrong: %d %d %d", s0, s2, s3)
	}
	out := rb.Drain()
	if len(out) != 4 {
		t.Fatalf("drained %d packets, want 4", len(out))
	}
	// Duplicate of an already committed sequence must be flagged.
	res := rb.Push(s0, []byte{99}, 0)
	if !res.Duplicate {
		t.Fatal("committed-sequence duplicate not detected")
	}
	// A stale packet behind the committed frontier is rejected and never
	// overwrites history. Frontier is at ext 65538 (wire 2); wire 65533
	// unwraps to ext 65533, strictly behind the frontier.
	res = rb.Push(uint16(65533), []byte{7}, 0)
	if res.Accepted || !res.Duplicate || !res.Late {
		t.Fatalf("stale packet misclassified: %+v", res)
	}
	st := rb.Snapshot()
	// Two packets landed behind the frontier: one explicit retransmission
	// of a committed seq and one stale seq; both are counted as duplicates
	// and late-dropped.
	if st.Duplicates != 2 || st.LateDropped != 2 {
		t.Fatalf("stats wrong: %+v", st)
	}
}

func TestGapExpiration(t *testing.T) {
	rb := transport.NewReorderBuffer(1, 1000)
	rb.Push(10, []byte("a"), 0)
	rb.Push(11, []byte("b"), 0)
	// hole at 12,13 then 14 arrives
	rb.Push(14, []byte("e"), 0)
	if out := rb.Drain(); len(out) != 2 {
		t.Fatalf("should drain only contiguous prefix, got %d", len(out))
	}
	if gaps := rb.ExpireGaps(500); len(gaps) != 0 {
		t.Fatal("gap expired before timeout")
	}
	gaps := rb.ExpireGaps(2000)
	if len(gaps) != 1 {
		t.Fatalf("gaps=%d want 1", len(gaps))
	}
	g := gaps[0]
	if g.ExtFirst != 12 || g.ExtLast != 13 || !g.TimeRange.Valid() {
		t.Fatalf("gap locator wrong: %+v", g)
	}
	fe := g.GapFault()
	if fe.Category != fault.CatPacketGap || fe.SeqFirst != 12 || fe.SeqLast != 13 {
		t.Fatalf("gap fault locator wrong: %v", fe)
	}
	// After expiration the buffered packet 14 drains immediately.
	if out := rb.Drain(); len(out) != 1 {
		t.Fatalf("post-gap drain wrong: %d", len(out))
	}
}

func TestJitterBufferHardLimitAndPause(t *testing.T) {
	rb := transport.NewReorderBuffer(1, 1_000_000_000)
	jb := transport.NewJitterBuffer(rb)
	jb.SetHardLimit(1024)
	big := make([]byte, 2048)
	if _, err := jb.Enqueue(1, big, 0); err == nil {
		t.Fatal("hard memory limit not enforced")
	}
	// Sustained low bandwidth must pause commits. The policy needs two
	// slow windows, each itself backed by two low observations.
	nominal := int64(10_000_000)
	for i := 0; i < 6; i++ {
		jb.ObserveBandwidth(1_000_000, nominal)
	}
	if !jb.Paused() {
		t.Fatal("buffer did not pause under sustained slowdown")
	}
	jb.Enqueue(20, []byte("x"), 0)
	if out := jb.Dequeue(); out != nil {
		t.Fatal("committed while paused")
	}
	// Recovery clears the pause and releases buffered packets.
	for i := 0; i < 4; i++ {
		jb.ObserveBandwidth(nominal, nominal)
	}
	if jb.Paused() {
		t.Fatal("buffer did not resume after bandwidth recovery")
	}
	if out := jb.Dequeue(); len(out) != 1 {
		t.Fatal("buffered packet not released on resume")
	}
	if jb.SoftLimit() > jb.HardLimit() {
		t.Fatal("soft limit crossed hard ceiling")
	}
}

func TestAssemblerAndDamagePropagation(t *testing.T) {
	spec := fixture.DefaultSpec(41)
	pkts, err := spec.BuildPackets(0)
	if err != nil {
		t.Fatalf("BuildPackets: %v", err)
	}
	asm := transport.NewAssembler(spec.SampleCount)
	rb := transport.NewReorderBuffer(1, 1_000_000_000)
	completed := map[uint32]bool{}
	for _, p := range pkts {
		var pkt rtp.Packet
		if err := pkt.Unmarshal(p.Raw); err != nil {
			t.Fatal(err)
		}
		res := rb.Push(pkt.SequenceNumber, pkt.Payload, 0)
		if res.Overflow {
			t.Fatal("reorder overflow")
		}
		for _, payload := range rb.Drain() {
			idx, done, ferr := asm.Ingest(payload)
			if ferr != nil {
				t.Fatalf("ingest: %v", ferr)
			}
			if done {
				if _, ok := asm.Materialize(idx); !ok {
					t.Fatalf("sample %d not materializable", idx)
				}
				completed[idx] = true
			}
		}
	}
	// DefaultSpec has no drops: every sample must complete.
	if len(completed) != spec.SampleCount {
		t.Fatalf("completed=%d want %d", len(completed), spec.SampleCount)
	}
}

func TestBurstLossPropagatesToSampleDamage(t *testing.T) {
	spec := fixture.DefaultSpec(41)
	// Drop an 8-packet burst beginning at sequence 30.
	spec.DropBurstAt = 30
	spec.DropBurstN = 8
	pkts, err := spec.BuildPackets(0)
	if err != nil {
		t.Fatal(err)
	}
	rb := transport.NewReorderBuffer(1, 1000)
	asm := transport.NewAssembler(spec.SampleCount)
	var damages []transport.SampleDamage
	feed := func(now int64) {
		for _, p := range pkts {
			var pkt rtp.Packet
			if err := pkt.Unmarshal(p.Raw); err != nil {
				t.Fatal(err)
			}
			rb.Push(pkt.SequenceNumber, pkt.Payload, now)
			for _, payload := range rb.Drain() {
				if _, _, ferr := asm.Ingest(payload); ferr != nil {
					t.Fatalf("ingest: %v", ferr)
				}
			}
			for _, g := range rb.ExpireGaps(now) {
				damages = append(damages, asm.MarkDamage(g))
			}
		}
		for _, g := range rb.ExpireGaps(now + 10_000) {
			damages = append(damages, asm.MarkDamage(g))
		}
	}
	feed(5000)
	if len(damages) == 0 {
		t.Fatal("burst loss produced no sample damage")
	}
	// Two fault classes must carry packet -> sample locators.
	for _, d := range damages {
		fe := d.DamageFault()
		if fe.Category != fault.CatSampleDamage {
			t.Fatalf("wrong category: %s", fe.Category)
		}
		if fe.SampleIndex < 0 || fe.SeqFirst < 0 || !fe.HasRange {
			t.Fatalf("damage missing locators: %v", fe)
		}
	}
	if asm.DamagedCount() == 0 {
		t.Fatal("no samples marked damaged")
	}
}

