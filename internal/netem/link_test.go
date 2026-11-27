package netem

import (
	"testing"

	"bcastqc/internal/media"
	rtpx "bcastqc/internal/rtp"
)

func makePackets(n int) []rtpx.Packet {
	out := make([]rtpx.Packet, n)
	for i := range out {
		out[i] = rtpx.Packet{
			Seq:       int64(i + 1),
			SendNS:    int64(i) * 1_000_000,
			TimeNS:    int64(i) * 1_000_000,
			Track:     media.TrackVideo,
			ClockRate: media.VideoClockRate,
			Raw:       make([]byte, 100),
		}
	}
	return out
}

func TestDeterministicAcrossRuns(t *testing.T) {
	run := func() int {
		l := NewLink(Config{
			Seed: 42, LossGood: 0.3, LossBad: 0.7, RecoverBad: 0.4,
			Duplicate: 0.1, Reorder: 0.1, OneWayDelayNS: 5_000_000,
		})
		got := 0
		for _, p := range makePackets(200) {
			if l.Accept(p, p.SendNS) {
				got++
			}
		}
		return len(l.Drain()) + got
	}
	if run() != run() {
		t.Fatal("same seed produced different traffic")
	}
}

func TestBurstLossClusters(t *testing.T) {
	// 一旦进入坏状态，连续高丢包率应产生成簇缺口。
	l := NewLink(Config{Seed: 7, LossGood: 0.02, LossBad: 0.95, RecoverBad: 0.1})
	var delivered []int64
	for _, p := range makePackets(500) {
		if l.Accept(p, p.SendNS) {
			delivered = append(delivered, p.Seq)
		}
	}
	if len(delivered) == 0 || len(delivered) == 500 {
		t.Fatalf("unexpected delivery count %d", len(delivered))
	}
	// 至少存在一个长度 >=2 的连续缺失簇。
	maxGap, prev := 0, int64(0)
	for _, s := range delivered {
		if gap := int(s - prev - 1); gap > maxGap {
			maxGap = gap
		}
		prev = s
	}
	if maxGap < 2 {
		t.Fatalf("no burst loss observed, max gap=%d", maxGap)
	}
}

func TestDuplicatesAndReorder(t *testing.T) {
	// 重复：关闭乱序，仅统计副本数量。
	dup := NewLink(Config{Seed: 99, Duplicate: 0.5, OneWayDelayNS: 1_000_000})
	for _, p := range makePackets(60) {
		dup.Accept(p, p.SendNS)
	}
	if len(dup.Drain()) <= 60 {
		t.Fatal("duplicates not injected")
	}

	// 乱序：关闭重复，检查交付序号回退。
	ro := NewLink(Config{Seed: 99, Reorder: 0.5, OneWayDelayNS: 1_000_000})
	for _, p := range makePackets(60) {
		ro.Accept(p, p.SendNS)
	}
	outOfOrder := false
	var seqs []int64
	for _, ev := range ro.Drain() {
		seqs = append(seqs, ev.Packet.Seq)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] < seqs[i-1] {
			outOfOrder = true
		}
	}
	if !outOfOrder {
		t.Fatalf("reorder did not produce out-of-order delivery: %v", seqs)
	}
}

func TestStallDefersDelivery(t *testing.T) {
	cfg := Config{
		Seed: 1, OneWayDelayNS: 1_000_000,
		StallStartNS: 5_000_000, StallEndNS: 20_000_000,
	}
	l := NewLink(cfg)
	for _, p := range makePackets(30) {
		l.Accept(p, p.SendNS)
	}
	for _, ev := range l.Drain() {
		if ev.Packet.SendNS >= 5_000_000 && ev.Packet.SendNS < 20_000_000 &&
			ev.ArriveNS < cfg.StallEndNS {
			t.Fatalf("stalled packet delivered early: send=%d arrive=%d",
				ev.Packet.SendNS, ev.ArriveNS)
		}
	}
}

func TestBandwidthVaries(t *testing.T) {
	cfg := Config{Seed: 3, BaseBPS: 100_000, BandJitter: 0.5, OneWayDelayNS: 1_000}
	l := NewLink(cfg)
	a := l.currentBPS(0)
	b := l.currentBPS(62_500_000) // 约 1/4 周期
	if a == b {
		t.Fatalf("bandwidth did not vary: %d", a)
	}
}
