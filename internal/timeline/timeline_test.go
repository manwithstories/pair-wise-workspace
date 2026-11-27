package timeline

import (
	"testing"

	"bcast/internal/fault"
	"bcast/internal/model"
)

// memSource 是可控的内存包源，避免 timeline 测试依赖 probe 实现。
type memSource struct {
	plan model.Plan
	pkts []model.Packet
	gaps []model.Gap
	pos  int
}

func (m *memSource) ReadPacket() (model.Packet, bool, error) {
	if m.pos >= len(m.pkts) {
		return model.Packet{}, false, nil
	}
	p := m.pkts[m.pos]
	m.pos++
	return p, true, nil
}
func (m *memSource) Plan() model.Plan  { return m.plan }
func (m *memSource) Gaps() []model.Gap { return m.gaps }

func oneTrackPlan(modulus uint64, timebase uint64, periodUS int64) model.Plan {
	tp := model.TrackPlan{ID: 0, Kind: model.KindVideo, Timebase: timebase, PeriodUS: periodUS}
	return model.Plan{
		DurationMS: 1000, Tracks: []model.TrackPlan{tp},
		WrapModulus: map[uint8]uint64{0: modulus},
	}
}

func pkt(seq, pts uint64, dur uint32) model.Packet {
	return model.Packet{TrackID: 0, Kind: model.KindVideo, Seq: seq,
		PTS: pts, Duration: dur, ByteStart: int64(seq) * 10, ByteEnd: int64(seq)*10 + 9}
}

func TestBuildUnwrapsNormalizesAndDedupes(t *testing.T) {
	// 时基 1000 滴答/秒；模数 8，PTS 序列在 6 之后回绕。
	src := &memSource{plan: oneTrackPlan(8, 1000, 1000)}
	src.pkts = []model.Packet{
		pkt(0, 4, 1), pkt(1, 5, 1),
		pkt(1, 5, 1),                             // 同序号同 PTS 的重复包 -> 去重
		pkt(2, 6, 1), pkt(3, 0, 1), pkt(4, 1, 1), // 6 -> 0 回绕
	}
	fc := &fault.Collector{}
	tl, err := Build(src, fc)
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Samples) != 5 {
		t.Fatalf("samples = %d, want 5 (one dupe removed)", len(tl.Samples))
	}
	if fc.CountByKind(fault.KindDupe) != 1 {
		t.Fatalf("dupe faults = %d, want 1", fc.CountByKind(fault.KindDupe))
	}
	// 展开后 PTS（滴答）：4,5,6,8,9；归一化到微秒（*1000）。
	wantPTS := []int64{4000, 5000, 6000, 8000, 9000}
	for i, s := range tl.Samples {
		if s.PTS != wantPTS[i] {
			t.Fatalf("sample %d PTS = %d, want %d", i, s.PTS, wantPTS[i])
		}
		if s.Duration != 1000 {
			t.Fatalf("sample %d duration = %d, want 1000", i, s.Duration)
		}
	}
	if tl.Samples[3].Wrap != 1 {
		t.Fatalf("wrap count = %d, want 1", tl.Samples[3].Wrap)
	}
}

func TestResolveGapsKeepsHalfOpenHole(t *testing.T) {
	src := &memSource{plan: oneTrackPlan(8, 1000, 1000)}
	// 序号 0、1 收到，2 丢失，3 收到：缺口 [seq2]。
	src.pkts = []model.Packet{pkt(0, 0, 1), pkt(1, 1, 1), pkt(3, 3, 1)}
	src.gaps = []model.Gap{{TrackID: 0, Kind: model.KindVideo, Missing: []uint64{2},
		AfterSeq: 1, BeforeSeq: 3}}
	fc := &fault.Collector{}
	tl, err := Build(src, fc)
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Gaps) != 1 {
		t.Fatalf("gaps = %d, want 1", len(tl.Gaps))
	}
	g := tl.Gaps[0]
	// 空洞起点 = seq1 末尾 = 1000+1000 = 2000us；终点 = seq3 起点 = 3000us。
	if g.StartPTS != 2000 || g.EndPTS != 3000 {
		t.Fatalf("gap interval = [%d,%d), want [2000,3000)", g.StartPTS, g.EndPTS)
	}
}

func TestEstimateDriftRecoversInjectedSlope(t *testing.T) {
	tp := model.TrackPlan{ID: 1, Kind: model.KindOpus, Timebase: 1_000_000, PeriodUS: 20_000}
	const ppm int64 = 250
	var samples []model.Sample
	for seq := int64(0); seq < 200; seq++ {
		expected := seq * tp.PeriodUS
		actual := expected + expected*ppm/1_000_000
		samples = append(samples, model.Sample{TrackID: 1, Seq: uint64(seq), PTS: actual})
	}
	est, ok := EstimateDrift(tp, samples)
	if !ok {
		t.Fatal("estimate expected to succeed")
	}
	if est.PPM != ppm {
		t.Fatalf("estimated ppm = %d, want %d", est.PPM, ppm)
	}
	if est.N != 200 {
		t.Fatalf("N = %d, want 200", est.N)
	}
}

func TestEstimateDriftWithDropsStillAligned(t *testing.T) {
	tp := model.TrackPlan{ID: 0, Kind: model.KindVideo, Timebase: 1_000_000, PeriodUS: 40_000}
	const ppm int64 = -100
	var samples []model.Sample
	for seq := int64(0); seq < 100; seq++ {
		if seq == 10 || seq == 11 { // 丢两个包，序号本身保留语义
			continue
		}
		expected := seq * tp.PeriodUS
		actual := expected + expected*ppm/1_000_000
		samples = append(samples, model.Sample{Seq: uint64(seq), PTS: actual})
	}
	est, ok := EstimateDrift(tp, samples)
	if !ok {
		t.Fatal("estimate expected to succeed")
	}
	if est.PPM != ppm {
		t.Fatalf("estimated ppm = %d, want %d (drops must not skew seq-based fit)", est.PPM, ppm)
	}
}

func TestEstimateDriftRejectsTinyInput(t *testing.T) {
	tp := model.TrackPlan{ID: 0, PeriodUS: 1000}
	if _, ok := EstimateDrift(tp, []model.Sample{{Seq: 0, PTS: 0}}); ok {
		t.Fatal("single sample must not produce an estimate")
	}
}
