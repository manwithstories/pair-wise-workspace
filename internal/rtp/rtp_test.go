package rtp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math/rand"
	"testing"

	"relay/internal/media"
)

// frame 组装一条 [4 字节长度][数据报] 记录。
func frame(b []byte) []byte {
	out := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	copy(out[4:], b)
	return out
}

// rtpPkt 构造最小 RTP 包（V=2, PT=96, 无扩展/填充）。
func rtpPkt(seq uint16, ts uint32, payload []byte) []byte {
	h := make([]byte, 12)
	h[0] = 0x80
	h[1] = 96
	binary.BigEndian.PutUint16(h[2:4], seq)
	binary.BigEndian.PutUint32(h[4:8], ts)
	binary.BigEndian.PutUint32(h[8:12], 0xAABBCCDD)
	return append(h, payload...)
}

// srPkt 构造 RTCP Sender Report（V=2, PT=200）。
func srPkt(ntpSec, ntpFrac, rtpTS uint32) []byte {
	b := make([]byte, rtcpSRMin)
	b[0] = 0x80 // V=2
	b[1] = 200  // 第二字节低 7 位即 PT 200，最高位 0 由解析侧按 PT≥128 识别
	binary.BigEndian.PutUint16(b[2:4], 6) // 28 字节 = 4 + 6*4
	binary.BigEndian.PutUint32(b[4:8], 0x11223344)
	binary.BigEndian.PutUint32(b[8:12], ntpSec)
	binary.BigEndian.PutUint32(b[12:16], ntpFrac)
	binary.BigEndian.PutUint32(b[16:20], rtpTS)
	return b
}

func TestReaderHappyPath(t *testing.T) {
	var stream bytes.Buffer
	stream.Write(frame(rtpPkt(7, 1000, []byte{1, 2, 3, 4})))
	stream.Write(frame(srPkt(9000, 0, 1000)))
	stream.Write(frame(rtpPkt(8, 1200, []byte{9})))

	r := NewReader(&stream)
	d1, err := r.Read()
	if err != nil || d1.Kind != KindRTP || d1.Seq != 7 || d1.Timestamp != 1000 ||
		len(d1.Payload) != 4 || d1.Offset != 0 {
		t.Fatalf("d1 mismatch: %+v err=%v", d1, err)
	}
	d2, err := r.Read()
	if err != nil || !d2.IsRTCP || d2.PayloadType != 200 || d2.SRNTPSeconds != 9000 ||
		d2.SRRTP != 1000 || d2.Offset != 4+16 {
		t.Fatalf("d2 SR mismatch: %+v err=%v", d2, err)
	}
	d3, err := r.Read()
	if err != nil || d3.Seq != 8 || d3.Offset != 4+16+4+28 {
		t.Fatalf("d3 offset/seq mismatch: %+v err=%v", d3, err)
	}
	if _, err := r.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF, got %v", err)
	}
	// 流尾部只剩半个长度前缀：按正常结束而非截断故障。
	r2 := NewReader(bytes.NewReader(append(frame(rtpPkt(0, 0, []byte{1})), 0, 0, 1)))
	if _, err := r2.Read(); err != nil {
		t.Fatalf("first dg: %v", err)
	}
	if _, err := r2.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("partial prefix want EOF, got %v", err)
	}
}

func TestReaderTruncationAndBadLength(t *testing.T) {
	badVersion := rtpPkt(2, 0, []byte{1})
	badVersion[0] = 0
	csrcOverflow := rtpPkt(3, 0, []byte{1})
	csrcOverflow[0] = 0x8F
	rtcpBadLen := srPkt(1, 0, 0)
	rtcpBadLen[3] = 9
	cases := map[string]struct {
		stream []byte
		kind   media.FaultKind
	}{
		"illegal small len":    {[]byte{0, 0, 0, 0}, media.FaultFraming},   // 声明长度 0 < 12
		"body truncated":       {frame(rtpPkt(1, 0, nil))[:10], media.FaultFraming},
		"bad version":          {frame(badVersion), media.FaultBadPayload},
		"csrc overflow":        {frame(csrcOverflow), media.FaultTruncated},
		"rtcp length mismatch": {frame(rtcpBadLen), media.FaultTruncated},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := NewReader(bytes.NewReader(c.stream))
			_, err := r.Read()
			f, ok := media.FindFault(err)
			if !ok || f.Kind != c.kind {
				t.Fatalf("%s: want fault %v, got %v", name, c.kind, err)
			}
			if f.Offset < 0 && name != "prefix truncated" {
				t.Fatalf("%s: missing byte offset: %+v", name, f)
			}
		})
	}
}

func TestReaderOffsetPrecision(t *testing.T) {
	// 变长包序列，校验每个 Fault/数据报的字节偏移精确到前缀起点。
	rng := rand.New(rand.NewSource(1))
	var stream bytes.Buffer
	var offsets []int64
	payloads := make([][]byte, 50)
	for i := range payloads {
		n := 4 + rng.Intn(80)
		payloads[i] = make([]byte, n)
		rng.Read(payloads[i])
		offsets = append(offsets, int64(stream.Len()))
		stream.Write(frame(rtpPkt(uint16(i), uint32(i*160), payloads[i])))
	}
	r := NewReader(&stream)
	for i := 0; i < 50; i++ {
		d, err := r.Read()
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if d.Offset != offsets[i] || d.Index != int64(i) {
			t.Fatalf("dg %d offset=%d want %d index=%d", i, d.Offset, offsets[i], d.Index)
		}
		if !bytes.Equal(d.Payload, payloads[i]) {
			t.Fatalf("dg %d payload corrupted", i)
		}
	}
}

func TestReorderInOrderAndDuplicate(t *testing.T) {
	ro := NewReorder(8)
	var faults []*media.Fault
	on := func(f *media.Fault) { faults = append(faults, f) }
	var got []uint16
	for i := 0; i < 10; i++ {
		got = append(got, seqs(ro.Push(&Datagram{Seq: uint16(i)}, on))...)
	}
	if len(got) != 10 || got[9] != 9 {
		t.Fatalf("emitted %v", got)
	}
	// 窗口内重复：缓存一个距 next 不远的乱序包，再重发同一序号，只计数不输出。
	ro.Push(&Datagram{Seq: 13, Index: 13}, on) // 距 next=10 的距离为 3，落在窗口内
	dup := ro.Push(&Datagram{Seq: 13, Index: 13}, on)
	if dup != nil {
		t.Fatal("duplicate must not be emitted")
	}
	// 已滑过序号的重发属于迟到，不是重复。
	ro.Push(&Datagram{Seq: 0, Index: 99}, on)
	st := ro.Stats()
	if st.Duplicates != 1 || st.Late != 1 || st.InOrder != 10 {
		t.Fatalf("stats=%+v", st)
	}
}

func TestReorderReorderAndLate(t *testing.T) {
	ro := NewReorder(4)
	var faults []*media.Fault
	on := func(f *media.Fault) { faults = append(faults, f) }
	// 到达顺序: 0,2,1,3 → 0 先输出，2/1 缓存，1 到达后释放 1,2，再 3。
	ro.Push(&Datagram{Seq: 0, Index: 0}, on)
	ro.Push(&Datagram{Seq: 2, Index: 2}, on)
	out := ro.Push(&Datagram{Seq: 1, Index: 1}, on)
	if seqs(out) == nil || len(out) != 2 || out[0].Seq != 1 || out[1].Seq != 2 {
		t.Fatalf("reorder release=%v", seqs(out))
	}
	ro.Push(&Datagram{Seq: 3, Index: 3}, on)
	if ro.Stats().Reordered < 1 {
		t.Fatalf("reordered count=%d", ro.Stats().Reordered)
	}
	// 迟到包：序号 0 已滑过。
	ro.Push(&Datagram{Seq: 0, Index: 99}, on)
	if ro.Stats().Late != 1 {
		t.Fatalf("late=%d", ro.Stats().Late)
	}
	// 超出窗口的过早包也进诊断。
	ro.Push(&Datagram{Seq: 100, Index: 100}, on)
	if ro.Stats().Late != 2 {
		t.Fatalf("out-of-window late=%d", ro.Stats().Late)
	}
	if len(faults) == 0 {
		t.Fatal("expected diagnostic faults")
	}
}

func TestReorderMissingAndFlush(t *testing.T) {
	ro := NewReorder(3)
	var missing []uint16
	on := func(f *media.Fault) {
		if f.Kind == media.FaultMissing {
			missing = append(missing, f.Seq)
		}
	}
	// 缺序号 1：连续推 0,2,3,4，窗口填满后 1 被判丢包。
	ro.Push(&Datagram{Seq: 0}, on)
	ro.Push(&Datagram{Seq: 2}, on)
	ro.Push(&Datagram{Seq: 3}, on)
	out := ro.Push(&Datagram{Seq: 4}, on)
	got := seqs(out)
	if len(got) != 3 || got[0] != 2 || got[2] != 4 {
		t.Fatalf("after missing release=%v", got)
	}
	if len(missing) != 1 || missing[0] != 1 {
		t.Fatalf("missing=%v", missing)
	}
	// 16 位回绕：从 65534 起。
	ro2 := NewReorder(4)
	var wrap []uint16
	for _, s := range []uint16{65534, 65535, 0, 1} {
		wrap = append(wrap, seqs(ro2.Push(&Datagram{Seq: s}, on))...)
	}
	if len(wrap) != 4 || wrap[0] != 65534 || wrap[3] != 1 {
		t.Fatalf("wrap sequence=%v", wrap)
	}
	// Flush 排空尾部缺口。
	ro3 := NewReorder(4)
	ro3.Push(&Datagram{Seq: 10}, on)
	ro3.Push(&Datagram{Seq: 12}, on)
	rest := ro3.Flush(on)
	if len(rest) != 1 || rest[0].Seq != 12 {
		t.Fatalf("flush=%v", seqs(rest))
	}
	if ro3.Stats().Missing != 1 {
		t.Fatalf("flush missing=%d", ro3.Stats().Missing)
	}
}

func TestReorderLocateFaultWithinOnePacket(t *testing.T) {
	// 坏包定位误差不超过 1 个包：丢弃 seq=50，重排器报告的 Missing 必须恰为 50。
	// 持续推进直到窗口填满，缺包会在 seq=67 到达时被宣布。
	ro := NewReorder(16)
	var miss []uint16
	on := func(f *media.Fault) {
		if f.Kind == media.FaultMissing {
			miss = append(miss, f.Seq)
		}
	}
	for i := 0; i < 80; i++ {
		if i == 50 {
			continue
		}
		ro.Push(&Datagram{Seq: uint16(i), Index: int64(i)}, on)
	}
	if len(miss) != 1 || miss[0] != 50 {
		t.Fatalf("located missing=%v want [50]", miss)
	}
}

func seqs(ds []*Datagram) []uint16 {
	var out []uint16
	for _, d := range ds {
		out = append(out, d.Seq)
	}
	return out
}
