package engine

import (
	"os"
	"path/filepath"
	"testing"

	"relay/internal/index"
	"relay/internal/media"
	"relay/internal/streamgen"
)

// TestFaultPropagationAcrossModules 验证坏包故障从 rtp 解析经 index 扫描
// 一路携带序号与字节偏移传播到引擎层，且时钟漂移（ppm）在同一索引中可观测。
// 这是 rtp→index→clock→engine 的跨模块端到端校验。
func TestFaultPropagationAcrossModules(t *testing.T) {
	dir := t.TempDir()
	const damage uint16 = 12000
	stream, _ := streamgen.Generate(streamgen.Spec{
		Seed: 41, DurationMS: 130000, AudioRate: 48000, Channels: 2,
		ClockRate: 48000, PPM: 150, DamageSeq: damage,
		DamageKind: streamgen.DamageBadPayload,
	})
	in := filepath.Join(dir, "in.bin")
	if err := os.WriteFile(in, stream, 0o644); err != nil {
		t.Fatal(err)
	}

	// index 层：故障必须带精确序号与非负字节偏移。
	f, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var propagated []media.Fault
	idx, err := index.Scan(f, params(), func(flt *media.Fault) {
		propagated = append(propagated, *flt)
	})
	if err != nil {
		t.Fatalf("recoverable fault must not abort scan: %v", err)
	}
	if len(propagated) == 0 {
		t.Fatal("fault did not propagate to scan callback")
	}
	var found *media.Fault
	for i := range idx.Faults {
		if idx.Faults[i].Seq == damage {
			found = &idx.Faults[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("damage seq %d missing from index faults: %+v", damage, idx.Faults)
	}
	if found.Offset < 0 {
		t.Fatalf("fault must carry byte offset: %+v", found)
	}
	// 时钟异常（150ppm 漂移）必须在同一索引中恢复，证明两个子系统共用时间轴。
	if idx.PPM <= 0 {
		t.Fatalf("drift not recovered alongside fault: ppm=%d", idx.PPM)
	}

	// engine 层：运行后定位结果必须与索引中的坏包序号一致（误差 0）。
	cfg := Config{
		InputPath: in, OutputPath: filepath.Join(dir, "out.pcm"),
		WorkDir: filepath.Join(dir, "work"), Params: params(),
	}
	rep, err := Run(cfg)
	if err != nil {
		t.Fatalf("engine run: %v", err)
	}
	rep.DamagePacket = int64(damage)
	if rep.LocatedPacket != int64(damage) {
		t.Fatalf("engine located=%d want %d", rep.LocatedPacket, damage)
	}
	rep.LocateDelta = rep.LocatedPacket - rep.DamagePacket
	if rep.LocateDelta != 0 {
		t.Fatalf("cross-module locate delta=%d", rep.LocateDelta)
	}
}
