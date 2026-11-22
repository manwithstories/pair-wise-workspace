package run

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mediaverify/internal/fault"
	"mediaverify/internal/fixture"
	"mediaverify/internal/manifest"
	"mediaverify/internal/scan"
)

func setupProgram(t *testing.T, seed int64, lossBurst int) (string, fixture.Options) {
	t.Helper()
	dir := t.TempDir()
	opts := fixture.DefaultOptions(seed)
	opts.LossBurst = lossBurst
	prog, err := fixture.Generate(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	mpd, _ := os.ReadFile(prog.MPDPath)
	mp, err := manifest.ParseDASH(prog.MPDPath, strings.NewReader(string(mpd)))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := scan.Build(mp, dir, prog.MPDPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Save(filepath.Join(dir, "index.json")); err != nil {
		t.Fatal(err)
	}
	return dir, opts
}

func baseParams(dir string) Params {
	return Params{
		Dir:            dir,
		ManifestPath:   filepath.Join(dir, "prog.mpd"),
		IndexPath:      filepath.Join(dir, "index.json"),
		OutputPath:     filepath.Join(dir, "out.mp4"),
		CheckpointPath: filepath.Join(dir, "checkpoint.json"),
		TargetKbps:     256,
		OutRate:        48000,
		OutChannels:    2,
		TargetLUFS:     -16,
		Sleep:          func(time.Duration) {},
	}
}

func loadPackets(t *testing.T, dir string, opts fixture.Options) []fixture.Packet {
	t.Helper()
	// 与 Generate 相同的确定性包流：重新生成并取 Packets
	prog, err := fixture.Generate(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	return prog.Packets
}

func TestNormalRunMetrics(t *testing.T) {
	dir, opts := setupProgram(t, 42, 4)
	p := baseParams(dir)
	p.Packets = loadPackets(t, dir, opts)
	res, err := Execute(p)
	if err != nil {
		t.Fatal(err)
	}
	m := res.Metrics
	if !m.IndexDigestMatch {
		t.Fatal("digest mismatch")
	}
	if m.GapPackets != 4 {
		t.Fatalf("gap=%d want 4", m.GapPackets)
	}
	if m.DuplicatePackets == 0 || m.ReorderedPackets == 0 {
		t.Fatalf("dup=%d reorder=%d", m.DuplicatePackets, m.ReorderedPackets)
	}
	if m.ManifestErrorMs != 0 {
		t.Fatalf("manifest error=%f ms", m.ManifestErrorMs)
	}
	if m.BitrateErrorPct < -10 || m.BitrateErrorPct > 10 {
		t.Fatalf("bitrate error=%f%%", m.BitrateErrorPct)
	}
	if m.OutputLUFS < -16.3 || m.OutputLUFS > -15.7 {
		t.Fatalf("output lufs=%f", m.OutputLUFS)
	}
	if m.PeakDBFS > -1 {
		t.Fatalf("peak=%f", m.PeakDBFS)
	}
	if m.RecoveryAction != "none" {
		t.Fatalf("recovery=%s", m.RecoveryAction)
	}
}

func TestDeterministicRepeatRuns(t *testing.T) {
	run := func() (string, string) {
		dir, opts := setupProgram(t, 42, 4)
		p := baseParams(dir)
		p.Packets = loadPackets(t, dir, opts)
		res, err := Execute(p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(p.OutputPath)
		return res.Metrics.JSON(), string(b)
	}
	j1, b1 := run()
	j2, b2 := run()
	if j1 != j2 {
		t.Fatalf("metrics differ:\n%s\n%s", j1, j2)
	}
	if b1 != b2 {
		t.Fatal("output bytes differ")
	}
}

func TestBandwidthDropKeepsTimeline(t *testing.T) {
	mk := func(drop bool) string {
		dir, opts := setupProgram(t, 42, 4)
		p := baseParams(dir)
		p.Packets = loadPackets(t, dir, opts)
		if drop {
			p.BandwidthKbps = 900
			p.BandwidthDropAt = 200
			p.BandwidthDropKbps = 90 // 带宽骤降
		}
		res, err := Execute(p)
		if err != nil {
			t.Fatal(err)
		}
		return res.Metrics.JSON()
	}
	if mk(false) != mk(true) {
		t.Fatal("bandwidth change altered media timeline or output")
	}
}

func TestTruncatedSegmentLocatable(t *testing.T) {
	dir := t.TempDir()
	cs, err := fixture.GenerateCorrupt(dir, fixture.DefaultOptions(7))
	if err != nil {
		t.Fatal(err)
	}
	// 用截断节目目录建索引并执行
	mpd, _ := os.ReadFile(filepath.Join(cs.TruncatedSegDir, "prog.mpd"))
	mp, err := manifest.ParseDASH("prog.mpd", strings.NewReader(string(mpd)))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := scan.Build(mp, cs.TruncatedSegDir, filepath.Join(cs.TruncatedSegDir, "prog.mpd"))
	if err != nil {
		t.Fatal(err)
	}
	ip := filepath.Join(cs.TruncatedSegDir, "index.json")
	if err := idx.Save(ip); err != nil {
		t.Fatal(err)
	}
	p := baseParams(cs.TruncatedSegDir)
	p.ManifestPath = filepath.Join(cs.TruncatedSegDir, "prog.mpd")
	p.IndexPath = ip
	p.Packets = nil
	_, err = Execute(p)
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindCorrupt || fe.Segment < 0 {
		t.Fatalf("err=%v", err)
	}
}

func TestTimestampRollbackError(t *testing.T) {
	dir, opts := setupProgram(t, 42, 0)
	cs, err := fixture.GenerateCorrupt(t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	p := baseParams(dir)
	p.Packets = cs.RollbackPackets
	_, err = Execute(p)
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindTransport {
		t.Fatalf("err=%v", err)
	}
}

func TestResumeAfterInterruption(t *testing.T) {
	// 完整运行基线
	dirA, opts := setupProgram(t, 42, 4)
	pA := baseParams(dirA)
	pA.Packets = loadPackets(t, dirA, opts)
	if _, err := Execute(pA); err != nil {
		t.Fatal(err)
	}
	baseline, _ := os.ReadFile(pA.OutputPath)

	// 中断运行：2 个分段后停止
	dirB, optsB := setupProgram(t, 42, 4)
	pB := baseParams(dirB)
	pB.Packets = loadPackets(t, dirB, optsB)
	pB.StopAfterSegments = 2
	_, err := Execute(pB)
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err=%v", err)
	}
	// 续跑
	pC := baseParams(dirB)
	pC.Packets = pB.Packets
	pC.Resume = true
	res, err := Execute(pC)
	if err != nil {
		t.Fatal(err)
	}
	if res.Metrics.RecoveryAction != "resumed" || res.Metrics.ResumedFromSeg != 2 {
		t.Fatalf("recovery=%s from=%d", res.Metrics.RecoveryAction, res.Metrics.ResumedFromSeg)
	}
	resumed, _ := os.ReadFile(pC.OutputPath)
	if string(baseline) != string(resumed) {
		t.Fatal("resumed output differs from uninterrupted baseline")
	}
}

func TestUntrustedCheckpointRescan(t *testing.T) {
	dir, opts := setupProgram(t, 42, 4)
	p := baseParams(dir)
	p.Packets = loadPackets(t, dir, opts)
	p.StopAfterSegments = 2
	if _, err := Execute(p); errors.Is(err, ErrInterrupted) != true {
		t.Fatalf("err=%v", err)
	}
	// 篡改检查点摘要 → 不可信 → 丢弃并重扫
	cb, _ := os.ReadFile(p.CheckpointPath)
	tampered := strings.Replace(string(cb), `"frames"`, `"framex"`, 1)
	if err := os.WriteFile(p.CheckpointPath, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	p2 := baseParams(dir)
	p2.Packets = p.Packets
	p2.Resume = true
	res, err := Execute(p2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Metrics.RecoveryAction != "rescan" {
		t.Fatalf("recovery=%s", res.Metrics.RecoveryAction)
	}
}
