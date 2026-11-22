package fieldcut

import (
	"bytes"
	"testing"

	"fieldcut/internal/parse"
	"fieldcut/internal/pipeline"
	"fieldcut/internal/testfixtures"
	"fieldcut/internal/timeline"
)

// 解析 -> go-mp4 回写 -> 再解析，核对样本时间戳与跨轨事件一致率。
func TestRoundtripTimestampsAndEvents(t *testing.T) {
	g := testfixtures.NewRNG(41)
	spec := testfixtures.DefaultSpecs()[0]
	data, _ := testfixtures.BuildMP4(g, spec)
	// 顶层追加一个 emsg 盒，验证跨轨事件随容器回写保留。
	emsgRaw := testfixtures.BuildEmsg(2000, spec.Timescale, 500, 3, "cue-x")
	full := append(append([]byte{}, data...), emsgRaw...)

	lim := parse.DefaultLimits()
	roots1, rep1 := parse.Walk(bytes.NewReader(full), lim)
	tis1, err := parse.ParseTracks(bytes.NewReader(full), roots1, lim, rep1)
	if err != nil || len(tis1) != 1 {
		t.Fatalf("first parse failed: %v", err)
	}
	ev1 := parse.ParseEvents(bytes.NewReader(full), roots1, tis1, rep1)

	// 回写。
	ws := newMemWriteSeeker()
	if err := parse.CopyContainer(bytes.NewReader(full), roots1, ws); err != nil {
		t.Fatalf("copy container: %v", err)
	}
	written := ws.bytes()

	// 再解析。
	roots2, rep2 := parse.Walk(bytes.NewReader(written), lim)
	tis2, err := parse.ParseTracks(bytes.NewReader(written), roots2, lim, rep2)
	if err != nil || len(tis2) != 1 {
		t.Fatalf("second parse failed: %v", err)
	}
	ev2 := parse.ParseEvents(bytes.NewReader(written), roots2, tis2, rep2)

	a, b := tis1[0].Samples, tis2[0].Samples
	if len(a) != len(b) {
		t.Fatalf("sample count changed after roundtrip: %d vs %d", len(a), len(b))
	}
	matched := 0
	for i := range a {
		if a[i].RawTick == b[i].RawTick && a[i].TimeNs == b[i].TimeNs {
			matched++
		}
	}
	rate := float64(matched) / float64(len(a))
	if rate < 0.98 {
		t.Fatalf("timestamp roundtrip rate %.3f below 0.98", rate)
	}
	if len(ev1) != len(ev2) {
		t.Fatalf("event count changed: %d vs %d", len(ev1), len(ev2))
	}
	for i := range ev1 {
		if ev1[i].EventID != ev2[i].EventID ||
			ev1[i].StartNs != ev2[i].StartNs || ev1[i].Text != ev2[i].Text {
			t.Fatalf("event %d mismatch after roundtrip", i)
		}
	}
}

// 时间戳回绕必须保持跨轨（视频+RTP 事件轨）时间线单调与关联正确。
func TestWrapCrossTrackAssociation(t *testing.T) {
	g := timeline.NewGraph()
	vc, _ := timeline.NewClock(1, timeline.KindVideo, 90000, 0)
	if err := g.Add(&timeline.Track{ID: 1, Kind: timeline.KindVideo, Clock: vc}); err != nil {
		t.Fatal(err)
	}
	rt, err := parse.NewRTPTrack(g, 2, timeline.KindEvent, 0x99, 90000)
	if err != nil {
		t.Fatal(err)
	}
	// 事件轨依赖视频轨，验证依赖图把媒体排在事件之前。
	tr, _ := g.Get(2)
	tr.Deps = []uint32{1}
	order, err := g.TopoSort()
	if err != nil {
		t.Fatalf("toposort: %v", err)
	}
	if order[0].ID != 1 || order[1].ID != 2 {
		t.Fatal("video must precede dependent event track")
	}

	series := testfixtures.WrapRTPSeries(90000)
	var prevNs int64
	first := true
	for _, p := range series {
		dg, err := parse.MarshalPacket(p.Seq, p.TS, 0x99, false, 96, []byte{1})
		if err != nil {
			t.Fatal(err)
		}
		pkt, err := rt.Ingest(dg)
		if err != nil {
			t.Fatalf("ingest across wrap: %v", err)
		}
		if !first && pkt.Ns < prevNs {
			t.Fatal("mapped timeline must stay monotonic across wrap")
		}
		prevNs, first = pkt.Ns, false
	}
	if rt.Wraps() == 0 {
		t.Fatal("wrap must be detected and expanded for cross-track association")
	}
}

// 索引缺页经重建后仍须产出安全切片（跨轨影响：降级标记可核验）。
func TestIndexGapEndToEnd(t *testing.T) {
	opt := DefaultOptions(41, t.TempDir())
	opt.Corruptions = []testfixtures.Corruption{testfixtures.CorruptIndexGap}
	res, err := RunFromGenerated(opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 3 || res.Report.HasFatal() {
		t.Fatal("index gap is recoverable, must not be fatal")
	}
	safe := 0
	for _, s := range res.Manifest.Segments {
		if s.SafeCut {
			safe++
		}
	}
	if safe == 0 {
		t.Fatal("rebuilt index must still yield safe cuts")
	}
}

// 健康路径全量指标满足验收阈值。
func TestHealthyRunMetrics(t *testing.T) {
	opt := DefaultOptions(41, t.TempDir())
	res, err := RunFromGenerated(opt)
	if err != nil {
		t.Fatal(err)
	}
	sum := res.Manifest.Summary
	if sum.RoundtripRate < 0.98 {
		t.Fatalf("roundtrip_rate %.3f", sum.RoundtripRate)
	}
	if sum.KeyframeHitRate < 0.95 {
		t.Fatalf("keyframe_hit_rate %.3f", sum.KeyframeHitRate)
	}
	if sum.Segments < 4 {
		t.Fatalf("segments %d", sum.Segments)
	}
	for id, h := range sum.SegmentSHA256 {
		if len(h) != 64 {
			t.Fatalf("hash for %s not 64 hex chars", id)
		}
	}
	if sum.SilenceMs <= 0 || sum.TimelineErrors < 0 {
		t.Fatal("silence/timeline fields missing")
	}
}

// 重跑 verify 必须逐片段哈希一致。
func TestVerifyHashesStable(t *testing.T) {
	dir := t.TempDir()
	opt := DefaultOptions(41, dir)
	if _, err := RunFromGenerated(opt); err != nil {
		t.Fatal(err)
	}
	res, err := Verify(dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("verify exit=%d", res.ExitCode)
	}
}

// 容器级损坏必须判致命（退出码 3），并保留错误而非静默继续。
func TestFatalContainerError(t *testing.T) {
	opt := DefaultOptions(41, t.TempDir())
	opt.Corruptions = []testfixtures.Corruption{testfixtures.CorruptContainer}
	res, err := RunFromGenerated(opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || res.Fatal == nil {
		t.Fatalf("container corruption must yield exit 3, got code=%d err=%v",
			res.ExitCode, res.Fatal)
	}
	if !res.Report.HasFatal() {
		t.Fatal("fatal error must be recorded in report")
	}
}

// 参数/输入错误由命令层映射为退出码 2，这里核验库对缺失目录的行为。
func TestVerifyMissingInput(t *testing.T) {
	res, err := Verify(t.TempDir() + "/does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 2 {
		t.Fatalf("missing input must be exit 2, got %d", res.ExitCode)
	}
}

var _ = pipeline.DefaultThresholds

// 可增长内存 WriteSeeker，供 go-mp4 Writer 使用。
type memWriteSeeker struct {
	data []byte
	pos  int64
}

func newMemWriteSeeker() *memWriteSeeker { return &memWriteSeeker{} }

func (w *memWriteSeeker) bytes() []byte { return w.data }

func (w *memWriteSeeker) Write(p []byte) (int, error) {
	end := w.pos + int64(len(p))
	if end > int64(len(w.data)) {
		grown := make([]byte, end)
		copy(grown, w.data)
		w.data = grown
	}
	copy(w.data[w.pos:end], p)
	w.pos = end
	return len(p), nil
}

func (w *memWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case 0:
		w.pos = offset
	case 1:
		w.pos += offset
	case 2:
		w.pos = int64(len(w.data)) + offset
	}
	return w.pos, nil
}
