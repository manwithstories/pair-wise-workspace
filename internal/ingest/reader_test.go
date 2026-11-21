package ingest

import (
	"crypto/sha256"
	"testing"

	"archivecheck/internal/fixture"
	"archivecheck/internal/model"
)

func goodFrames(t *testing.T, n int) []model.Frame {
	t.Helper()
	p := fixture.Params{
		Frames: n, Reps: []string{"r"}, WrapAt: 1 << 32, CorruptFrame: -1,
		KeyInterval: 10, AudioRate: 48000, RTPClock: 90000,
	}
	return fixture.Generate(p)["r"]
}

func TestVerifyCleanPrefix(t *testing.T) {
	frames := goodFrames(t, 200)
	r := NewReader("r", 1<<32)
	prefix, err := r.Verify(frames)
	if err != nil {
		t.Fatalf("干净序列不应报错: %v", err)
	}
	if len(prefix) != len(frames) {
		t.Fatalf("可信前缀长度错误: %d/%d", len(prefix), len(frames))
	}
}

func TestVerifyHashMismatch(t *testing.T) {
	frames := goodFrames(t, 200)
	frames[50].Payload[0] ^= 0x01
	r := NewReader("r", 1<<32)
	prefix, err := r.Verify(frames)
	if err == nil || err.Seq != 50 || err.Fault != model.FaultHashMismatch {
		t.Fatalf("错误定位错误: %+v", err)
	}
	if len(prefix) != 50 {
		t.Fatalf("应保留 50 帧可信前缀, got %d", len(prefix))
	}
}

func TestVerifyMetaMissing(t *testing.T) {
	frames := goodFrames(t, 100)
	frames[30].PayloadHash = nil
	r := NewReader("r", 1<<32)
	_, err := r.Verify(frames)
	if err == nil || err.Seq != 30 || err.Fault != model.FaultMetaMissing {
		t.Fatalf("元数据缺失定位错误: %+v", err)
	}
}

func TestVerifyClockBackward(t *testing.T) {
	frames := goodFrames(t, 100)
	// 制造严格超过半程的原始时间戳跳变，触发回绕器倒退判定；
	// 恰好等于半程按 RFC3550 属于前向，因此额外加一个步进。
	// 跳变帧更新内部基准，倒退标志在其后第一个正常步进帧处检出。
	frames[40].RTPTimestamp = frames[39].RTPTimestamp + 1<<31 + 3000
	r := NewReader("r", 1<<32)
	_, err := r.Verify(frames)
	if err == nil || err.Seq != 41 || err.Fault != model.FaultClockBack {
		t.Fatalf("倒退定位错误: %+v", err)
	}
}

func TestRepairGaps(t *testing.T) {
	frames := goodFrames(t, 100)
	gaps := []model.Gap{{Start: 10, End: 12}, {Start: 50, End: 51}}
	results := Repair(frames, gaps)
	if len(results) != len(frames) {
		t.Fatalf("结果数错误: %d", len(results))
	}
	for _, cr := range results {
		wantLost := (cr.Seq >= 10 && cr.Seq < 12) || cr.Seq == 50
		if cr.OK == wantLost {
			t.Fatalf("帧 %d 丢失标记错误: OK=%v", cr.Seq, cr.OK)
		}
		if wantLost && cr.Fault != model.FaultLost {
			t.Fatalf("帧 %d 故障码错误: %s", cr.Seq, cr.Fault)
		}
	}
	MarkRecovered(results, map[uint32]bool{20: true})
	if !results[20].Repaired || !results[20].OK {
		t.Fatal("恢复标记未生效")
	}
}

func TestHashSizeConstant(t *testing.T) {
	if sha256.Size != 32 {
		t.Fatal("SHA-256 摘要长度应为 32")
	}
}
