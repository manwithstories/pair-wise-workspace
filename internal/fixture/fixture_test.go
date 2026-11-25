package fixture

import (
	"os"
	"path/filepath"
	"testing"

	"relaycheck/internal/index"
	"relaycheck/internal/manifest"
	"relaycheck/internal/model"
	mp4pkg "relaycheck/internal/mp4"
	"relaycheck/internal/rtptrace"
)

func TestGenerateDeterministic(t *testing.T) {
	spec := DefaultSpec(17)
	d1 := t.TempDir()
	d2 := t.TempDir()
	if err := Generate(d1, spec, false); err != nil {
		t.Fatal(err)
	}
	if err := Generate(d2, spec, false); err != nil {
		t.Fatal(err)
	}
	for seq := 0; seq < spec.SegmentCount; seq++ {
		a, _ := os.ReadFile(filepath.Join(d1, segName(seq)))
		b, _ := os.ReadFile(filepath.Join(d2, segName(seq)))
		if filepath.Join(d1, segName(seq)) == "" || string(a) != string(b) {
			t.Fatalf("segment %d not deterministic", seq)
		}
	}
}

func TestCleanFixturesParseEndToEnd(t *testing.T) {
	dir := t.TempDir()
	spec := DefaultSpec(17)
	if err := Generate(dir, spec, false); err != nil {
		t.Fatal(err)
	}
	hls, err := manifest.ParseHLSFile(filepath.Join(dir, "playlist.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	dash, err := manifest.ParseDASHFile(filepath.Join(dir, "manifest.mpd"))
	if err != nil {
		t.Fatal(err)
	}
	if len(hls.Refs) != spec.SegmentCount || len(dash.Refs) != spec.SegmentCount {
		t.Fatalf("hls=%d dash=%d", len(hls.Refs), len(dash.Refs))
	}
	cross := manifest.CrossCheck(dir, hls, dash)
	if len(cross.Faults) != 0 {
		t.Fatalf("clean fixtures must have no manifest faults: %+v", cross.Faults)
	}
	mem := model.NewMemTracker(1 << 20)
	for seq := 0; seq < spec.SegmentCount; seq++ {
		rep, err := mp4pkg.ParseFragment(filepath.Join(dir, segName(seq)), seq, spec.TimeScale, mem)
		if err != nil {
			t.Fatalf("frag %d: %v", seq, err)
		}
		if len(rep.Samples) != spec.SamplesPerSeg {
			t.Fatalf("frag %d samples=%d", seq, len(rep.Samples))
		}
		if len(rep.Faults) != 0 {
			t.Fatalf("clean frag %d faults: %+v", seq, rep.Faults)
		}
		tr, err := rtptrace.ParseFile(filepath.Join(dir, rtpName(seq)), seq, mem, 0)
		if err != nil {
			t.Fatalf("rtp %d: %v", seq, err)
		}
		if len(tr.Packets) == 0 {
			t.Fatalf("rtp %d no packets", seq)
		}
	}
}

func TestEdgeFixturesInjectFaults(t *testing.T) {
	dir := t.TempDir()
	spec := DefaultSpec(17)
	if err := Generate(dir, spec, true); err != nil {
		t.Fatal(err)
	}
	// 缺片：seg5 物理文件不存在。
	if _, err := os.Stat(filepath.Join(dir, segName(missingSeg))); !os.IsNotExist(err) {
		t.Fatal("missing segment file should not exist")
	}
	// 截断盒：seg7 解析出截断故障且定位到偏移。
	mem := model.NewMemTracker(1 << 20)
	trep, err := mp4pkg.ParseFragment(filepath.Join(dir, segName(truncatedSeg)), truncatedSeg, spec.TimeScale, mem)
	if err != nil {
		t.Fatal(err)
	}
	if len(trep.Faults) == 0 || trep.FirstBadOff < 0 {
		t.Fatalf("truncated frag faults=%+v", trep.Faults)
	}
	// 乱序/回绕：seg9 出现 reorder/wrap/loss 故障。
	tr, err := rtptrace.ParseFile(filepath.Join(dir, rtpName(reorderSeg)), reorderSeg, mem, 0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, f := range tr.Faults {
		kinds[f.Kind] = true
	}
	if !kinds[model.FaultRTPReorder] || !kinds[model.FaultRTPWrap] || !kinds[model.FaultRTPLoss] {
		t.Fatalf("expected reorder/wrap/loss, got %+v", tr.Faults)
	}
}

func TestCorruptCheckpointRejected(t *testing.T) {
	dir := t.TempDir()
	if err := Generate(dir, DefaultSpec(17), true); err != nil {
		t.Fatal(err)
	}
	if _, err := index.ReadValid(filepath.Join(dir, "corrupt-checkpoint.json")); err == nil {
		t.Fatal("corrupt checkpoint must fail validation")
	}
}

func TestOversizedSegmentBounded(t *testing.T) {
	dir := t.TempDir()
	if err := Generate(dir, DefaultSpec(17), true); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, segName(oversizedSeg)))
	if err != nil {
		t.Fatal(err)
	}
	// 用小于该分片的内存预算，必须触发 ErrMemoryLimit。
	mem := model.NewMemTracker(info.Size() - 100)
	_, err = mp4pkg.ParseFragment(filepath.Join(dir, segName(oversizedSeg)), oversizedSeg, 90000, mem)
	if err != model.ErrMemoryLimit {
		t.Fatalf("err = %v, want ErrMemoryLimit", err)
	}
}
