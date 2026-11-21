package transport

import "testing"

func redEvery10(seq uint32) bool { return seq%10 == 0 }

func TestLossRecoveryWithRedundancy(t *testing.T) {
	seqs := make([]uint32, 1000)
	for i := range seqs {
		seqs[i] = uint32(i)
	}
	pkts := Inject(seqs, redEvery10, FaultConfig{LossEvery: 97})
	w := NewWindow(32)
	for _, p := range pkts {
		w.Push(p)
	}
	stats, gaps := w.Flush()
	if stats.Injected == 0 {
		t.Fatal("应统计到注入故障")
	}
	if rate := stats.RecoveryRate(); rate < 0.95 {
		t.Fatalf("恢复率 %.3f 低于 0.95", rate)
	}
	// 不可恢复区间必须按半开区间记录且互不重叠。
	for i, g := range gaps {
		if g.Start >= g.End {
			t.Fatalf("非法区间 %+v", g)
		}
		if i > 0 && g.Start < gaps[i-1].End {
			t.Fatalf("区间重叠: %+v %+v", gaps[i-1], g)
		}
	}
}

func TestReorderReassemblesInWindow(t *testing.T) {
	seqs := make([]uint32, 60)
	for i := range seqs {
		seqs[i] = uint32(i)
	}
	pkts := Inject(seqs, redEvery10, FaultConfig{ReorderEvery: 5})
	w := NewWindow(32)
	for _, p := range pkts {
		w.Push(p)
	}
	stats, gaps := w.Flush()
	if len(gaps) != 0 {
		t.Fatalf("纯乱序不应有不可恢复区间: %+v", gaps)
	}
	if stats.ReordersSeen == 0 {
		t.Fatal("应观测到乱序包")
	}
}

func TestDuplicatePackets(t *testing.T) {
	seqs := make([]uint32, 40)
	for i := range seqs {
		seqs[i] = uint32(i)
	}
	pkts := Inject(seqs, redEvery10, FaultConfig{DupeEvery: 7})
	w := NewWindow(16)
	for _, p := range pkts {
		w.Push(p)
	}
	stats, gaps := w.Flush()
	if stats.Duplicates == 0 {
		t.Fatal("应识别重复包")
	}
	if len(gaps) != 0 {
		t.Fatalf("重复包不应造成丢失区间: %+v", gaps)
	}
}

func TestUnrecoverableLossRecorded(t *testing.T) {
	// 同帧的数据包与 FEC 伴随包都丢失时，必须形成显式不可恢复区间。
	seqs := []uint32{0, 1, 2, 3}
	pkts := Inject(seqs, redEvery10, FaultConfig{})
	w := NewWindow(4)
	// 帧 1 的两个包（序号 2、3）全部缺失，其余正常交付。
	w.Push(pkts[0])
	w.Push(pkts[1])
	w.Push(pkts[4])
	w.Push(pkts[5])
	w.Push(pkts[6])
	w.Push(pkts[7])
	stats, gaps := w.Flush()
	if stats.Unrecovered != 1 {
		t.Fatalf("同帧双丢按帧计 1 个不可恢复帧, got %d", stats.Unrecovered)
	}
	if len(gaps) != 1 || gaps[0].Start != 1 || gaps[0].End != 2 {
		t.Fatalf("不可恢复区间错误: %+v", gaps)
	}
}

func TestNoFaultsFullRecovery(t *testing.T) {
	seqs := make([]uint32, 50)
	for i := range seqs {
		seqs[i] = uint32(i)
	}
	pkts := Inject(seqs, redEvery10, FaultConfig{})
	w := NewWindow(8)
	for _, p := range pkts {
		w.Push(p)
	}
	stats, gaps := w.Flush()
	if stats.Injected != 0 || len(gaps) != 0 {
		t.Fatalf("无故障场景必须零丢失: %+v %+v", stats, gaps)
	}
	if stats.RecoveryRate() != 1 {
		t.Fatal("无故障恢复率应为 1")
	}
}
