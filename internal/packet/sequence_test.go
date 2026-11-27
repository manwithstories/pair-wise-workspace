package packet

import (
	"errors"
	"testing"

	"archivemedia/internal/media"
)

func sampleEvent(seq uint16, payload string) *Event {
	// 显式复制，模拟每个网络包拥有独立后备数组，
	// 避免字符串字面量在闭包间共享底层数组导致测试假阳性。
	p := make([]byte, len(payload))
	copy(p, payload)
	e := &Event{
		StreamID:      1,
		Epoch:         0,
		Seq:           seq,
		Codec:         CodecOpus,
		RTPTimestamp:  uint64(seq) * 960,
		ClockRate:     48000,
		DurationTicks: 960,
		ArrivalMicros: int64(seq) * 20000,
		ArrivalIndex:  int(seq),
		Payload:       p,
	}
	return e
}

func TestRecordRoundtripAndCRC(t *testing.T) {
	e := sampleEvent(7, "hello-packet")
	b, err := e.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(b) != headerLen+len(e.Payload) {
		t.Fatalf("记录长度 = %d", len(b))
	}
	got, err := UnmarshalEvent(b)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Seq != 7 || string(got.Payload) != "hello-packet" || got.CRC == 0 {
		t.Fatalf("往返字段丢失: %+v", got)
	}
	// 破坏载荷字节必须被 CRC 拦截。
	bad := append([]byte(nil), b...)
	bad[headerLen] ^= 0xFF
	if _, err := UnmarshalEvent(bad); !media.IsKind(err, media.KindPacketCorrupt) {
		t.Fatalf("损坏载荷未被识别: %v", err)
	}
	// 截断记录必须被拒收。
	if _, err := UnmarshalEvent(b[:headerLen-1]); err == nil {
		t.Fatal("截断记录必须报错")
	}
}

func TestValidateRejectsBadEvents(t *testing.T) {
	big := make([]byte, MaxPayload+1)
	oversize := sampleEvent(1, "x")
	oversize.Payload = big
	if _, err := oversize.MarshalBinary(); !media.IsKind(err, media.KindPacketCorrupt) {
		t.Fatal("越界载荷必须拒收")
	}
	zero := sampleEvent(2, "x")
	zero.DurationTicks = 0
	if err := zero.Validate(); !media.IsKind(err, media.KindPacketCorrupt) {
		t.Fatal("零时长必须拒收")
	}
	unknown := sampleEvent(3, "x")
	unknown.Codec = CodecUnknown
	if err := unknown.Validate(); err == nil {
		t.Fatal("未知编码必须拒收")
	}
	empty := sampleEvent(4, "x")
	empty.Payload = nil
	if err := empty.Validate(); err == nil {
		t.Fatal("空载荷必须拒收")
	}
	// 占位包允许空载荷但要求非零时长。
	ph := sampleEvent(5, "")
	ph.Flags = FlagMissing
	ph.Payload = nil
	if err := ph.Validate(); err != nil {
		t.Fatalf("合法占位包被拒: %v", err)
	}
}

func TestExtendSeqWraparound(t *testing.T) {
	prev := int64(65534)
	cases := []struct {
		seq  uint16
		want int64
	}{
		{65535, 65535},
		{0, 65536},
		{1, 65537},
		{65534, 65534}, // 前序包（迟到）回到旧值
	}
	for _, c := range cases {
		if got := ExtendSeq(prev, c.seq); got != c.want {
			t.Fatalf("ExtendSeq(%d,%d)=%d want %d", prev, c.seq, got, c.want)
		}
	}
	if d := SeqDistance(0, 65535); d != 1 {
		t.Fatalf("回绕正向距离 = %d want 1", d)
	}
	if d := SeqDistance(65534, 0); d != -2 {
		t.Fatalf("回绕反向距离 = %d want -2", d)
	}
}

func TestTSUnwrapper(t *testing.T) {
	u := &TSUnwrapper{}
	got := u.Push(0xFFFFFFF0)
	if got != 0xFFFFFFF0 {
		t.Fatal("首包应原样返回")
	}
	// 0x10 - 0xFFFFFFF0（模 2^32）= 32，回绕后展开值 = 0xFFFFFFF0 + 32。
	got = u.Push(0x00000010)
	if got != 0x100000010 {
		t.Fatalf("时间戳回绕展开 = %#x want 0x100000010", got)
	}
	// 迟到旧时间戳：有向差为负，展开值必须能回退到回绕前区间。
	got = u.Push(0xFFFFFFF5)
	if got != 0xFFFFFFF5 {
		t.Fatalf("迟到时间戳回退 = %#x want 0xFFFFFFF5", got)
	}
	// 回到新周期，展开值继续单调。
	got = u.Push(0x00000020)
	if got != 0x100000020 {
		t.Fatalf("回绕后推进 = %#x want 0x100000020", got)
	}
}

func TestDeduperExactDuplicate(t *testing.T) {
	d := NewDeduper()
	a := sampleEvent(10, "same")
	b := sampleEvent(10, "same")
	b.ArrivalIndex = 99
	dec, err := d.Observe(a)
	if dec != DecisionAccepted || err != nil {
		t.Fatalf("首包判定 = %d %v", dec, err)
	}
	dec, err = d.Observe(b)
	if dec != DecisionDuplicate || err != nil {
		t.Fatalf("重复包判定 = %d %v", dec, err)
	}
	if d.Duplicates() != 1 || d.Len() != 1 {
		t.Fatal("重复统计错误")
	}
}

func TestDeduperConflictDeterministic(t *testing.T) {
	// 同键异内容：无论到达顺序如何，必须选择同一胜者。
	// 未序列化事件 CRC 为 0，规则退化为载荷字典序：字典序较小者胜。
	// 两个独立去重器分别模拟两种到达顺序，避免闭包变量捕获干扰。
	dFirst := NewDeduper()
	_, err := dFirst.Observe(sampleEvent(20, "aaaa"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = dFirst.Observe(sampleEvent(20, "other"))
	if !IsConflict(err) {
		t.Fatalf("期望冲突，得到 %v", err)
	}

	dSecond := NewDeduper()
	_, err = dSecond.Observe(sampleEvent(20, "other"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = dSecond.Observe(sampleEvent(20, "aaaa"))
	if !IsConflict(err) {
		t.Fatalf("期望冲突，得到 %v", err)
	}

	got1, _ := dFirst.Get(Key{1, 0, 20})
	got2, _ := dSecond.Get(Key{1, 0, 20})
	if string(got1.Payload) != string(got2.Payload) {
		t.Fatalf("选择随到达顺序变化: %q vs %q", got1.Payload, got2.Payload)
	}
	if string(got1.Payload) != "aaaa" {
		t.Fatalf("字典序规则应选择 aaaa，实际 %q", got1.Payload)
	}
	if len(dFirst.Conflicts()) != 1 || len(dSecond.Conflicts()) != 1 {
		t.Fatal("冲突必须记录")
	}
}

func TestDeduperConflictCRCRule(t *testing.T) {
	// 序列化后 CRC 非零时，CRC 较小者胜，且与到达顺序无关。
	mk := func(payload string) *Event {
		e := sampleEvent(21, payload)
		b, err := e.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		dec, err := UnmarshalEvent(b)
		if err != nil {
			t.Fatal(err)
		}
		return dec
	}
	run := func(first, second string) string {
		d := NewDeduper()
		_, _ = d.Observe(mk(first))
		_, err := d.Observe(mk(second))
		if !IsConflict(err) {
			t.Fatalf("期望冲突，得到 %v", err)
		}
		got, _ := d.Get(Key{1, 0, 21})
		return string(got.Payload)
	}
	one := run("payload-aaa", "payload-bbb")
	two := run("payload-bbb", "payload-aaa")
	if one != two {
		t.Fatalf("CRC 规则随到达顺序变化: %q vs %q", one, two)
	}
}

func TestDeduperSnapshotSorted(t *testing.T) {
	d := NewDeduper()
	for _, seq := range []uint16{5, 1, 65535, 2} {
		if _, err := d.Observe(sampleEvent(seq, "p")); err != nil {
			t.Fatal(err)
		}
	}
	snap := d.Snapshot()
	want := []uint16{1, 2, 5, 65535}
	for i, e := range snap {
		if e.Seq != want[i] {
			t.Fatalf("快照顺序 = %v want %v", seqs(snap), want)
		}
	}
}

func TestConflictErrorKind(t *testing.T) {
	d := NewDeduper()
	_, _ = d.Observe(sampleEvent(30, "x"))
	_, err := d.Observe(sampleEvent(30, "y"))
	if !IsConflict(err) || !media.IsKind(err, media.KindPacketCorrupt) {
		t.Fatalf("冲突类型错误: %v", err)
	}
	var c *ConflictError
	if !errors.As(err, &c) || c.Chosen == nil || c.Rejected == nil {
		t.Fatal("冲突缺少双方事件")
	}
}

func seqs(es []*Event) []uint16 {
	out := make([]uint16, len(es))
	for i, e := range es {
		out[i] = e.Seq
	}
	return out
}
