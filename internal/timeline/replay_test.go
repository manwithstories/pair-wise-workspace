package timeline

import (
	"testing"

	"archivemedia/internal/packet"
)

func mkEvent(extSeq int64, payload string) *packet.Event {
	return &packet.Event{
		StreamID: 1, Epoch: 0, Seq: uint16(extSeq & 0xFFFF),
		Codec: packet.CodecOpus, ClockRate: 48000,
		RTPTimestamp:  uint64(extSeq) * 960,
		DurationTicks: 960, ArrivalMicros: extSeq * 20000,
		ArrivalIndex: int(extSeq), Payload: []byte(payload),
	}
}

func TestBurstLossPlaceholders(t *testing.T) {
	r := NewReplayer(48000)
	// 收到 0,1，突发丢失 2,3,4，再收到 5。
	for _, seq := range []int64{0, 1} {
		if _, err := r.Insert(seq, mkEvent(seq, "p")); err != nil {
			t.Fatal(err)
		}
	}
	tmpl := *mkEvent(2, "")
	n, err := r.FillMissing(2, 4, tmpl)
	if err != nil || n != 3 {
		t.Fatalf("占位数 = %d, err=%v", n, err)
	}
	if _, err := r.Insert(5, mkEvent(5, "p")); err != nil {
		t.Fatal(err)
	}
	ranges := r.MissingRanges()
	if len(ranges) != 1 || ranges[0] != [2]int64{2, 4} {
		t.Fatalf("丢失区间 = %v", ranges)
	}
	edge, ok := r.ContiguousEdge()
	if !ok || edge != 5 {
		t.Fatalf("连续边界 = %d,%v", edge, ok)
	}
	if r.Len() != 6 || r.MissingCount() != 3 {
		t.Fatalf("槽位统计错误: len=%d missing=%d", r.Len(), r.MissingCount())
	}
}

func TestRetransmissionReplacesPlaceholder(t *testing.T) {
	r := NewReplayer(48000)
	tmpl := *mkEvent(3, "")
	if _, err := r.FillMissing(3, 3, tmpl); err != nil {
		t.Fatal(err)
	}
	slot, _ := r.slots[3]
	if !slot.Missing {
		t.Fatal("初始应为占位")
	}
	replaced, err := r.Insert(3, mkEvent(3, "repaired"))
	if err != nil {
		t.Fatal(err)
	}
	if !replaced {
		t.Fatal("重传替换必须返回 replaced=true")
	}
	slot, _ = r.slots[3]
	if slot.Missing || string(slot.Event.Payload) != "repaired" {
		t.Fatalf("占位未被真实包替换: %+v", slot)
	}
	if r.MissingCount() != 0 {
		t.Fatal("替换后不应再有占位")
	}
}

func TestRewindAndReplayIdempotent(t *testing.T) {
	r := NewReplayer(48000)
	for seq := int64(0); seq < 10; seq++ {
		if _, err := r.Insert(seq, mkEvent(seq, "p")); err != nil {
			t.Fatal(err)
		}
	}
	// 检查点在序号 5，回溯丢弃 6..9。
	dropped := r.Rewind(5)
	if dropped != 4 || r.Len() != 6 {
		t.Fatalf("回溯丢弃 = %d, len=%d", dropped, r.Len())
	}
	edge, _ := r.ContiguousEdge()
	if edge != 5 {
		t.Fatalf("回溯后边界 = %d", edge)
	}
	// 重放 6..9，结果应与回溯前完全一致。
	for seq := int64(6); seq < 10; seq++ {
		if _, err := r.Insert(seq, mkEvent(seq, "p")); err != nil {
			t.Fatal(err)
		}
	}
	if r.Len() != 10 {
		t.Fatalf("重放后长度 = %d", r.Len())
	}
	slots := r.SortedSlots()
	for i, s := range slots {
		if s.ExtSeq != int64(i) || s.Missing {
			t.Fatalf("重放后槽位 %d 异常: %+v", i, s)
		}
	}
}

func TestFillMissingDoesNotOverwriteReal(t *testing.T) {
	r := NewReplayer(48000)
	if _, err := r.Insert(4, mkEvent(4, "real")); err != nil {
		t.Fatal(err)
	}
	tmpl := *mkEvent(0, "")
	n, err := r.FillMissing(2, 6, tmpl)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 { // 2,3,5,6 为占位，4 保留
		t.Fatalf("新建占位 = %d want 4", n)
	}
	if got := r.slots[4].Event.Payload; string(got) != "real" {
		t.Fatalf("真实槽位被覆盖: %q", got)
	}
}

func TestSortedSlotsOrder(t *testing.T) {
	r := NewReplayer(48000)
	for _, seq := range []int64{8, 2, 5} {
		if _, err := r.Insert(seq, mkEvent(seq, "p")); err != nil {
			t.Fatal(err)
		}
	}
	slots := r.SortedSlots()
	want := []int64{2, 5, 8}
	for i, s := range slots {
		if s.ExtSeq != want[i] {
			t.Fatalf("排序错误: 得 %v 想 %v", extSeqs(slots), want)
		}
	}
}

func extSeqs(ss []*Slot) []int64 {
	out := make([]int64, len(ss))
	for i, s := range ss {
		out[i] = s.ExtSeq
	}
	return out
}
