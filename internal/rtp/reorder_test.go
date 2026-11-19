package rtp

import (
	"testing"

	"github.com/pion/rtp"
)

func recOf(device string, seq uint16, ts uint32, payload []byte, arrival int64) CaptureRecord {
	p := &rtp.Packet{Header: rtp.Header{
		Version: 2, PayloadType: PayloadType, SequenceNumber: seq,
		Timestamp: ts, SSRC: 1,
	}, Payload: append([]byte(nil), payload...)}
	raw, _ := p.Marshal()
	return CaptureRecord{DeviceID: device, ArrivalNano: arrival, Packet: raw, HeaderOffset: arrival}
}

func mutatePayload(p []byte) []byte {
	out := append([]byte(nil), p...)
	if len(out) > 0 {
		out[len(out)/2] ^= 0xff
	}
	return out
}

func TestDuplicateConflictReportsTwoOffsets(t *testing.T) {
	r := NewReorderer("d", DefaultConfig())
	payload := []byte{1, 2, 3, 4}
	r1 := recOf("d", 100, 1000, payload, 1)
	r1.FileOffset = 111
	r2 := recOf("d", 100, 1000, payload, 2) // 同内容重复
	r2.FileOffset = 222
	r3 := recOf("d", 100, 1000, mutatePayload(payload), 3) // 冲突副本
	r3.FileOffset = 333

	if _, conflicts, err := r.Push(r1); err != nil || len(conflicts) != 0 {
		t.Fatalf("first push conflicts=%d err=%v", len(conflicts), err)
	}
	if _, conflicts, _ := r.Push(r2); len(conflicts) != 0 {
		t.Fatal("identical duplicate must not be a conflict")
	}
	_, conflicts, _ := r.Push(r3)
	if len(conflicts) != 1 {
		t.Fatalf("want 1 conflict, got %d", len(conflicts))
	}
	c := conflicts[0]
	if c.FirstOffset != 111 || c.SecondOffset != 333 {
		t.Fatalf("conflict must carry both offsets, got %+v", c)
	}
	st := r.Stats()
	if st.Duplicates != 2 || st.Conflicts != 1 {
		t.Fatalf("stats wrong: %+v", st)
	}
}

func TestReorderAndBurstGap(t *testing.T) {
	r := NewReorderer("d", DefaultConfig())
	// 先灌入足够多的连续包制造窗口，再插入一次乱序与一个突发缺口。
	var allConflicts []Conflict
	for seq := uint16(0); seq < 60; seq++ {
		if seq == 40 {
			continue // 模拟单包突发丢失
		}
		order := seq
		if seq == 21 {
			order = 22 // 交换 21/22 的到达顺序
		} else if seq == 22 {
			order = 21
		}
		_, cf, err := r.Push(recOf("d", order, uint32(order)*24,
			[]byte{byte(seq)}, int64(seq)+1))
		if err != nil {
			t.Fatal(err)
		}
		allConflicts = append(allConflicts, cf...)
	}
	_, gaps := r.Flush()
	st := r.Stats()
	if st.Reordered < 1 {
		t.Fatalf("reordered must be >=1, got %d", st.Reordered)
	}
	if len(gaps) == 0 {
		t.Fatal("burst loss must produce a gap")
	}
	found := false
	for _, g := range gaps {
		if g.MissingPackets >= 1 && g.EstSamples > 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("gap must estimate missing samples: %+v", gaps)
	}
	_ = allConflicts
}

func TestCacheLimitCheckpointAndForceAdvance(t *testing.T) {
	var checkpointCalls int
	cfg := Config{
		MaxCacheBytes: 200, BaseWaitPackets: 8, MinWaitPackets: 2, MaxWaitPackets: 16,
		OnCheckpoint: func(deviceID string, cached int64) error {
			checkpointCalls++
			return nil
		},
	}
	r := NewReorderer("d", cfg)
	// 每包约 40+ 字节，缓存 200 字节只能容纳少数包；制造缺口触发强制推进。
	for seq := uint16(0); seq < 40; seq++ {
		if seq == 10 || seq == 11 {
			continue // 缺口，迫使窗口在缓存压力下声明 gap
		}
		payload := make([]byte, 60)
		if _, _, err := r.Push(recOf("d", seq, uint32(seq)*24, payload, int64(seq))); err != nil {
			t.Fatal(err)
		}
	}
	if checkpointCalls == 0 {
		t.Fatal("cache overflow must invoke checkpoint callback")
	}
	out, gaps := r.Flush()
	if len(out) == 0 {
		t.Fatal("force-advance must still deliver packets")
	}
	if len(gaps) == 0 {
		t.Fatal("overflow across a hole must declare gap")
	}
}

func TestBandwidthChangesWaitBudget(t *testing.T) {
	cfg := DefaultConfig()
	cfg.BaseWaitPackets = 32
	r := NewReorderer("d", cfg)
	// 密集到达（高带宽）与稀疏到达（低带宽）交替。
	now := int64(0)
	for i := 0; i < 200; i++ {
		if (i/20)%2 == 0 {
			now += 1_000_000 // 快
		} else {
			now += 50_000_000 // 慢
		}
		rec := recOf("d", uint16(i), uint32(i)*24, make([]byte, 100), now)
		if _, _, err := r.Push(rec); err != nil {
			t.Fatal(err)
		}
	}
	st := r.Stats()
	if st.BandwidthBps <= 0 {
		t.Fatal("bandwidth must be estimated")
	}
	if st.WaitPackets < cfg.MinWaitPackets || st.WaitPackets > cfg.MaxWaitPackets {
		t.Fatalf("wait budget out of bounds: %d", st.WaitPackets)
	}
}
