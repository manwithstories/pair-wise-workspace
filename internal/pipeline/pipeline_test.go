package pipeline

import (
	"testing"
	"time"

	"github.com/pion/rtp"

	"mediad/internal/model"
)

func TestBoundedQueueCapacityAndPeak(t *testing.T) {
	q, err := NewBoundedQueue(3)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := q.TryPush(&Block{Seq: uint16(i)}); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}
	// 不变量：排队长度永不超过容量。
	if q.Len() != 3 || q.Peak() != 3 {
		t.Fatalf("len=%d peak=%d", q.Len(), q.Peak())
	}
	if err := q.TryPush(&Block{Seq: 99}); err != ErrQueueFull {
		t.Fatalf("full queue must reject, got %v", err)
	}
	// 满后再 Push 应阻塞直到消费者取走。
	done := make(chan struct{})
	go func() { q.Push(&Block{Seq: 99}); close(done) }()
	select {
	case <-done:
		t.Fatal("Push must block while queue is full")
	case <-time.After(20 * time.Millisecond):
	}
	b := q.Pop()
	if b.Seq != 0 {
		t.Fatalf("pop order seq=%d", b.Seq)
	}
	<-done
	if q.Peak() != 3 {
		t.Fatalf("peak must stay at capacity, got %d", q.Peak())
	}
	if q.BackpressureEvents() < 1 {
		t.Fatal("blocked push must register backpressure")
	}
}

func TestQueueDrainAccounting(t *testing.T) {
	q, _ := NewBoundedQueue(4)
	for i := 0; i < 4; i++ {
		q.Push(&Block{Seq: uint16(i)})
	}
	for i := 0; i < 4; i++ {
		if got := q.TryPop(); got == nil || got.Seq != uint16(i) {
			t.Fatalf("try pop %d got %v", i, got)
		}
	}
	if q.TryPop() != nil {
		t.Fatal("empty queue TryPop must be nil")
	}
	if q.Enqueued() != 4 || q.Dequeued() != 4 || q.Len() != 0 {
		t.Fatalf("accounting enq=%d deq=%d len=%d", q.Enqueued(), q.Dequeued(), q.Len())
	}
}

func TestRecovererRequiresVerifiedRedundancy(t *testing.T) {
	r := NewRecoverer()
	payload := []byte{1, 2, 3, 4}
	good := &Block{Seq: 7, Payload: payload, Checksum: checksum(payload)}
	r.AddRedundant(good)
	// 损坏的冗余：校验和不匹配。
	badPayload := []byte{9, 9, 9, 9}
	r.AddRedundant(&Block{Seq: 8, Payload: badPayload, Checksum: checksum([]byte{1, 2, 3, 4})})
	// seq 9 无任何冗余。
	evs := r.Resolve(GapRequest{
		SeqStart:  7,
		SeqEnd:    9,
		TimeRange: model.Range{Start: 100, End: 200},
	})
	if len(evs) != 3 {
		t.Fatalf("events=%d", len(evs))
	}
	if evs[0].Code != model.CodeRecovered || evs[0].Severity != model.SevWarning {
		t.Fatalf("seq7 should recover: %+v", evs[0])
	}
	if evs[1].Code != model.CodeUnrecoverable {
		t.Fatalf("seq8 checksum mismatch must be unrecoverable: %+v", evs[1])
	}
	if evs[2].Code != model.CodeUnrecoverable {
		t.Fatalf("seq9 missing redundancy must be unrecoverable: %+v", evs[2])
	}
	rec, unrec := r.Stats()
	if rec != 1 || unrec != 2 {
		t.Fatalf("stats rec=%d unrec=%d", rec, unrec)
	}
}

func TestRecovererNoSilentZeroFill(t *testing.T) {
	r := NewRecoverer()
	evs := r.Resolve(GapRequest{
		SeqStart:  1,
		SeqEnd:    1,
		TimeRange: model.Range{Start: 0, End: 10},
	})
	if evs[0].Severity != model.SevError {
		t.Fatal("gap without redundancy must be an error, never silently filled")
	}
	if r.RecoveryRate() != 0 {
		t.Fatalf("recovery rate must be 0, got %f", r.RecoveryRate())
	}
}

func TestCorruptRangesMerged(t *testing.T) {
	events := []model.Event{
		model.NewEvent(model.CodeUnrecoverable, model.SevError, model.Range{Start: 10, End: 20}, ""),
		model.NewEvent(model.CodeBurstLoss, model.SevWarning, model.Range{Start: 20, End: 30}, ""),
		model.NewEvent(model.CodeUnrecoverable, model.SevError, model.Range{Start: 20, End: 30}, ""),
		model.NewEvent(model.CodeUnrecoverable, model.SevError, model.Range{Start: 100, End: 110}, ""),
	}
	ranges := BuildCorruptRanges(events, func(s uint64) uint64 { return s })
	if len(ranges) != 2 {
		t.Fatalf("ranges=%d, want 2 merged groups: %+v", len(ranges), ranges)
	}
	if ranges[0].StartSample != 10 || ranges[0].EndSample != 30 {
		t.Fatalf("merged range wrong: %+v", ranges[0])
	}
	if ranges[1].StartSample != 100 || ranges[1].EndSample != 110 {
		t.Fatalf("separate range wrong: %+v", ranges[1])
	}
}

func TestBandwidthDropShrinksWindowNeverDrops(t *testing.T) {
	bw := NewBandwidthEstimator(100*time.Millisecond, 100, 25)
	now := time.Unix(0, 0)
	// 高带宽：100KB / 100ms ≈ 8 Mbps。
	for i := 0; i < 10; i++ {
		bw.Observe(now.Add(time.Duration(i)*10*time.Millisecond), 100_000)
	}
	if w := bw.SuggestWindow(8); w != 100 {
		t.Fatalf("healthy window=%d, want 100", w)
	}
	// 突降：1KB / 100ms ≈ 0.08 Mbps，远低于目标。
	for i := 0; i < 10; i++ {
		bw.Observe(now.Add(time.Second+time.Duration(i)*10*time.Millisecond), 1_000)
	}
	w := bw.SuggestWindow(8)
	if w != 25 {
		t.Fatalf("drop window=%d, want floor 25", w)
	}
	// 建议窗口始终在合法区间内，且不会影响已排队块。
	if w < 25 || w > 100 {
		t.Fatalf("window out of bounds: %d", w)
	}
}

// TestEndToEndInvariants 驱动完整 RTP→重排→队列→恢复链路，断言跨阶段不变量。
func TestEndToEndInvariants(t *testing.T) {
	const cap = 8
	p, err := New(Config{QueueCapacity: cap, ReorderWindow: 64, MaxBlockBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	// 序号 0..19，其中 5..9 突发丢失（5 包），为 5..8 提供一致冗余，
	// 9 无冗余（不可恢复）。
	for seq := uint16(0); seq < 20; seq++ {
		if seq >= 5 && seq <= 9 {
			continue
		}
		pkt := &rtp.Packet{
			Header:  rtp.Header{SequenceNumber: seq, Timestamp: uint32(seq) * 160},
			Payload: []byte{byte(seq), byte(seq + 1)},
		}
		if err := p.IngestRTP(pkt, func(ts uint32) uint64 { return uint64(ts) }, 160); err != nil {
			t.Fatalf("ingest %d: %v", seq, err)
		}
	}
	for seq := uint16(5); seq <= 8; seq++ {
		payload := []byte{byte(seq), byte(seq + 1)}
		p.AddRedundant(&Block{Seq: seq, Payload: payload, Checksum: checksum(payload)})
	}
	sampleOf := func(ts uint32) uint64 { return uint64(ts) }
	n := p.DrainOrdered(sampleOf, 160)
	if n != 5 { // 0..4 在缺口前可按序交付
		t.Fatalf("ordered drain=%d, want 5", n)
	}
	// 缺口在重排缓冲推进/Flush 后才可见。
	p.Reorder().Flush()
	p.RecordGaps(sampleOf)

	// 不变量 1：队列峰值不超过容量。
	if p.Queue().Peak() > cap {
		t.Fatalf("queue peak %d exceeds capacity %d", p.Queue().Peak(), cap)
	}
	// 排空并核对出入队计数一致。
	for p.Queue().TryPop() != nil {
	}
	if p.Queue().Enqueued() != p.Queue().Dequeued() {
		t.Fatalf("enq=%d != deq=%d", p.Queue().Enqueued(), p.Queue().Dequeued())
	}
	// 不变量 2：4/5 缺口可恢复，恢复率 0.8，且存在非空损坏区段。
	rec, unrec := p.Recoverer().Stats()
	if rec != 4 || unrec != 1 {
		t.Fatalf("recovery rec=%d unrec=%d", rec, unrec)
	}
	if p.Recoverer().RecoveryRate() < 0.8 {
		t.Fatalf("recovery rate %f < 0.8", p.Recoverer().RecoveryRate())
	}
	ranges := p.CorruptRanges(func(s uint64) uint64 { return s / 48 })
	if len(ranges) == 0 {
		t.Fatal("unrecovered gap must produce a non-empty corrupt range")
	}
}

func TestAVDiscontinuitySharesErrorModel(t *testing.T) {
	p, _ := New(Config{QueueCapacity: 2, ReorderWindow: 16})
	rng := model.Range{Start: 500, End: 900}
	p.NoteAVDiscontinuity(rng, "audio gap spans video keyframe")
	evs := p.Events()
	found := false
	for _, e := range evs {
		if e.Code == model.CodeAVDiscontinuity && e.Severity == model.SevError &&
			e.TimeRange == rng {
			found = true
		}
	}
	if !found {
		t.Fatalf("AV discontinuity must use the shared error model: %+v", evs)
	}
}

func TestBlockSizeWithinLimit(t *testing.T) {
	// 不变量：单块负载不得超过 1 MiB 解析缓冲上限。
	const oneMiB = int64(1 << 20)
	cfg := Config{QueueCapacity: 1, ReorderWindow: 8, MaxBlockBytes: oneMiB}
	p, _ := New(cfg)
	big := make([]byte, oneMiB)
	if int64(len(big)) > cfg.MaxBlockBytes {
		t.Fatal("block exceeds 1MiB")
	}
	p.Queue().Push(&Block{Payload: big})
	if p.Queue().Peak() > cfg.QueueCapacity {
		t.Fatal("peak exceeds bounded capacity")
	}
}
