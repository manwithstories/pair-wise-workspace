package rtptrace

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/pion/rtp"

	"relaycheck/internal/model"
)

// frame 生成一条长度前缀记录。
func frame(p *rtp.Packet) []byte {
	raw, err := p.Marshal()
	if err != nil {
		panic(err)
	}
	out := make([]byte, 4+len(raw))
	binary.BigEndian.PutUint32(out[:4], uint32(len(raw)))
	copy(out[4:], raw)
	return out
}

func pkt(seq uint16, ts uint32) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version: 2, PayloadType: 96,
			SequenceNumber: seq, Timestamp: ts, SSRC: 1,
		},
		Payload: []byte{0xAA, 0xBB},
	}
}

func join(pkts ...*rtp.Packet) []byte {
	var b bytes.Buffer
	for _, p := range pkts {
		b.Write(frame(p))
	}
	return b.Bytes()
}

func faultKinds(tr Trace) map[string]int {
	m := map[string]int{}
	for _, f := range tr.Faults {
		m[f.Kind]++
	}
	return m
}

func TestParseInOrder(t *testing.T) {
	src := join(pkt(10, 9000), pkt(11, 12000), pkt(12, 15000))
	tr, err := Parse(bytes.NewReader(src), 0, model.NewMemTracker(1<<20), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Packets) != 3 {
		t.Fatalf("packets = %d", len(tr.Packets))
	}
	if len(tr.Faults) != 0 {
		t.Fatalf("unexpected faults: %+v", tr.Faults)
	}
	if tr.Packets[2].ExtSeq != 12 {
		t.Fatalf("ext seq = %d", tr.Packets[2].ExtSeq)
	}
}

func TestSequenceWrap(t *testing.T) {
	src := join(pkt(0xFFFE, 0), pkt(0xFFFF, 3000), pkt(0, 6000), pkt(1, 9000))
	tr, err := Parse(bytes.NewReader(src), 2, model.NewMemTracker(1<<20), 0)
	if err != nil {
		t.Fatal(err)
	}
	k := faultKinds(tr)
	if k[model.FaultRTPWrap] != 1 {
		t.Fatalf("wrap faults = %d (%+v)", k[model.FaultRTPWrap], tr.Faults)
	}
	// 回绕后扩展序号应为 1<<16 + 0、1<<16 + 1。
	if tr.Packets[2].ExtSeq != 1<<16 || tr.Packets[3].ExtSeq != 1<<16+1 {
		t.Fatalf("ext seqs after wrap = %d,%d", tr.Packets[2].ExtSeq, tr.Packets[3].ExtSeq)
	}
	// 回绕不是丢包：不应报 loss。
	if k[model.FaultRTPLoss] != 0 {
		t.Fatalf("spurious loss at wrap: %+v", tr.Faults)
	}
}

func TestReorderAndLoss(t *testing.T) {
	// 100,102（丢 101）,101（乱序）
	src := join(pkt(100, 0), pkt(102, 6000), pkt(101, 3000))
	tr, err := Parse(bytes.NewReader(src), 1, model.NewMemTracker(1<<20), 0)
	if err != nil {
		t.Fatal(err)
	}
	k := faultKinds(tr)
	if k[model.FaultRTPLoss] != 1 || k[model.FaultRTPReorder] != 1 {
		t.Fatalf("loss=%d reorder=%d faults=%+v", k[model.FaultRTPLoss], k[model.FaultRTPReorder], tr.Faults)
	}
	// 乱序包的扩展序号保持 101，不产生新回绕。
	if tr.Packets[2].ExtSeq != 101 {
		t.Fatalf("reordered ext seq = %d", tr.Packets[2].ExtSeq)
	}
}

func TestTimestampJump(t *testing.T) {
	src := join(pkt(1, 0), pkt(2, 3000), pkt(3, 90_000_000))
	tr, err := Parse(bytes.NewReader(src), 0, model.NewMemTracker(1<<20), 0)
	if err != nil {
		t.Fatal(err)
	}
	k := faultKinds(tr)
	if k[model.FaultRTPTimestampJump] != 1 {
		t.Fatalf("jump faults = %d (%+v)", k[model.FaultRTPTimestampJump], tr.Faults)
	}
}

func TestTruncatedRecord(t *testing.T) {
	src := join(pkt(1, 0), pkt(2, 0))
	// 砍掉最后 3 字节，使长度前缀无法被满足。
	_, err := Parse(bytes.NewReader(src[:len(src)-3]), 0, model.NewMemTracker(1<<20), 0)
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("err = %v, want truncation", err)
	}
}

func TestPacketCountLimitAndMemory(t *testing.T) {
	src := join(pkt(1, 0), pkt(2, 0), pkt(3, 0))
	if _, err := Parse(bytes.NewReader(src), 0, model.NewMemTracker(1<<20), 2); err == nil {
		t.Fatal("expected packet count limit error")
	}
	// 内存预算小于单条记录（RTP 头 12 + 负载 2 = 14 字节）。
	if _, err := Parse(bytes.NewReader(src), 0, model.NewMemTracker(8), 0); err != model.ErrMemoryLimit {
		t.Fatalf("err = %v, want ErrMemoryLimit", err)
	}
}

func TestMapToTimeline(t *testing.T) {
	// 90kHz 时钟的 90000 ticks = 1s = 1e9 ns。
	if got := MapToTimeline(90000, 90000).RoundNanos(); got != 1_000_000_000 {
		t.Fatalf("mapped ns = %d", got)
	}
	if MapToTimeline(1, 0).IsValid() {
		t.Fatal("zero clock rate must yield invalid time")
	}
}

func TestReport(t *testing.T) {
	src := join(pkt(5, 0), pkt(6, 0))
	tr, err := Parse(bytes.NewReader(src), 0, model.NewMemTracker(1<<20), 0)
	if err != nil {
		t.Fatal(err)
	}
	rep := tr.Report()
	if rep.Packets != 2 || rep.MinSeq != 5 || rep.MaxExtSeq != 6 {
		t.Fatalf("report = %+v", rep)
	}
}
