package ingest

import (
	"testing"

	"medpipe/internal/clock"
)

func testConfig() Config {
	return Config{WindowSize: 4, MaxBufferBytes: 4096, PacketFrames: 2, TotalFrames: 10, BytesPerFrame: 2}
}

func mkPacket(seq int64, b byte) *Packet {
	pcm := []byte{b, b, b, b} // 2 帧 * 2 字节
	return &Packet{Seq: seq, BlockID: seq, StartFrame: seq * 2, Frames: 2,
		Source: SourcePCM, SHA256: HashPacket(pcm), PCM: pcm}
}

func TestReorderInOrderAndDrain(t *testing.T) {
	r, _ := New(testConfig())
	for i := int64(0); i < 3; i++ {
		if err := r.Push(mkPacket(i, byte(i+1))); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}
	got := r.Drain()
	if len(got) != 3 || got[2].Seq != 2 || r.Expect() != 3 {
		t.Fatalf("连续提交错误: n=%d expect=%d", len(got), r.Expect())
	}
}

func TestReorderOutOfOrder(t *testing.T) {
	r, _ := New(testConfig())
	order := []int64{2, 0, 1}
	for _, s := range order {
		if err := r.Push(mkPacket(s, byte(s+1))); err != nil {
			t.Fatalf("push %d: %v", s, err)
		}
	}
	got := r.Drain()
	if len(got) != 3 || got[0].Seq != 0 || got[2].Seq != 2 {
		t.Fatalf("乱序后应按序号提交: %+v", got)
	}
}

func TestDuplicateIdempotentAndConflict(t *testing.T) {
	r, _ := New(testConfig())
	p := mkPacket(1, 9)
	if err := r.Push(p); err != nil {
		t.Fatal(err)
	}
	if err := r.Push(mkPacket(1, 9)); err != nil || r.Duplicates() != 1 {
		t.Fatalf("相同重复包应幂等: err=%v dup=%d", err, r.Duplicates())
	}
	// 同序号不同内容必须冲突
	bad := mkPacket(1, 7)
	if err := r.Push(bad); clock.CodeOf(err) != clock.CodeConflict {
		t.Fatalf("冲突包应报 conflict, 得 %v", err)
	}
}

func TestLatePacketRejected(t *testing.T) {
	r, _ := New(testConfig())
	for i := int64(0); i < 3; i++ {
		_ = r.Push(mkPacket(i, 1))
	}
	r.Drain() // 水位推进到 3
	if err := r.Push(mkPacket(1, 1)); clock.CodeOf(err) != clock.CodeLatePacket || r.Late() != 1 {
		t.Fatalf("迟到包应被拒绝: %v late=%d", err, r.Late())
	}
}

func TestWindowAndMemoryLimits(t *testing.T) {
	cfg := testConfig()
	r, _ := New(cfg)
	// 水位 0，序号 4 距水位 4，恰越界（窗口 4 允许 0..3）
	if err := r.Push(mkPacket(4, 1)); clock.CodeOf(err) != clock.CodeOversize {
		t.Fatalf("超窗应报 oversize, 得 %v", err)
	}
	// 内存上限：小到容不下一个窗口的配置应直接拒绝
	cfg.MaxBufferBytes = 5
	if _, err := New(cfg); clock.CodeOf(err) != clock.CodeOversize {
		t.Fatalf("无法容纳一窗的内存配置应报错, 得 %v", err)
	}
	// 内存约束独立验证：绕过 New 构造极小缓冲，第二个包必须被拒绝
	memR := &Reorder{
		cfg:     Config{WindowSize: 8, MaxBufferBytes: 5, PacketFrames: 2, TotalFrames: 20, BytesPerFrame: 2},
		pending: map[int64]*Packet{},
	}
	if err := memR.Push(mkPacket(0, 1)); err != nil {
		t.Fatal(err)
	}
	if err := memR.Push(mkPacket(1, 1)); clock.CodeOf(err) != clock.CodeOversize {
		t.Fatalf("内存超限应报 oversize, 得 %v", err)
	}
}

func TestFlushSilenceFill(t *testing.T) {
	cfg := testConfig()
	cfg.TotalFrames = 20
	r, _ := New(cfg)
	_ = r.Push(mkPacket(0, 1))
	_ = r.Push(mkPacket(2, 3)) // 缺序号 1
	out, err := r.FlushThrough(3)
	if err != nil || len(out) != 3 {
		t.Fatalf("FlushThrough: n=%d err=%v", len(out), err)
	}
	if out[1].Source != SourceSilence || len(out[1].PCM) != 4 {
		t.Fatalf("空洞应补静音: %+v", out[1])
	}
	if out[2].Source != SourcePCM || r.Expect() != 3 {
		t.Fatal("真实包与水位异常")
	}
	// 继续推进到 6：剩余位置同样按静音补齐
	if _, err := r.FlushThrough(6); err != nil {
		t.Fatalf("尾部静音补偿失败: %v", err)
	}
	r3 := r
	if _, err := r3.FlushThrough(99); clock.CodeOf(err) != clock.CodeInvalidParam {
		t.Fatalf("越界静音应拒绝, 得 %v", err)
	}
}

func TestSimulateFaults(t *testing.T) {
	pkts := make([]*Packet, 12)
	for i := range pkts {
		pkts[i] = mkPacket(int64(i), byte(i+1))
	}
	// burst-loss：应有两段突发丢失
	_, _, st, err := Simulate(pkts, FaultPlan{Mode: FaultBurstLoss, Seed: 41, WindowSize: 4})
	if err != nil || len(st.Dropped) < 4 {
		t.Fatalf("burst-loss 统计异常: drop=%d err=%v", len(st.Dropped), err)
	}
	// duplicate：至少出现一次重复，且重放内容相同
	arrive, _, st, err := Simulate(pkts, FaultPlan{Mode: FaultDuplicate, Seed: 41, WindowSize: 4})
	if err != nil || len(st.Duplicates) == 0 || len(arrive) <= len(pkts) {
		t.Fatalf("duplicate 模拟异常: %+v err=%v", st, err)
	}
	// conflict：应产出内容不同的冲突包
	arrive, conflicts, _, err := Simulate(pkts, FaultPlan{Mode: FaultConflict, Seed: 41, WindowSize: 4})
	if err != nil || len(conflicts) != 1 || conflicts[0].SHA256 == pkts[conflicts[0].Seq].SHA256 {
		t.Fatalf("conflict 模拟异常: n=%d err=%v", len(conflicts), err)
	}
	// 确定性：同种子两次结果一致
	a1, c1, s1, _ := Simulate(pkts, FaultPlan{Mode: FaultFlapping, Seed: 7, WindowSize: 4})
	a2, c2, s2, _ := Simulate(pkts, FaultPlan{Mode: FaultFlapping, Seed: 7, WindowSize: 4})
	if len(a1) != len(a2) || len(c1) != len(c2) || s1.Reordered != s2.Reordered {
		t.Fatal("弱网模拟不具确定性")
	}
	_ = arrive
}
