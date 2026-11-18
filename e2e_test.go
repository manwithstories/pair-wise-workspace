package main

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestEndToEndFixture 用夹具生成中等时长样例，跑完整 analyze，
// 断言真值恢复精度、输出行数与 JSON/CSV 双写。
func TestEndToEndFixture(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.jsonl")
	jp := filepath.Join(dir, "out.json")
	cp := filepath.Join(dir, "out.csv")

	truth, err := Generate(GenConfig{Seed: 7, Seconds: 300, OutPath: in})
	if err != nil {
		t.Fatal(err)
	}
	if truth.BadAnchors == 0 || truth.Anchors < 6 {
		t.Fatalf("fixture must contain anchors and anomalies: %+v", truth)
	}
	// 注：300s 不足 32 位计数器回绕（视频 2^32µs≈4295s、音频≈5592s）；
	// 回绕由 unwrap_test 单测与 7200s 演示共同覆盖。

	summary, err := Analyze(AnalyzeOptions{
		InPath: in, JSONOut: jp, CSVOut: cp,
		Target: Timebase{1, 1_000_000_000},
	})
	if err != nil {
		t.Fatalf("analyze failed: %v", err)
	}
	f := summary.Fits[summary.Epochs[0]]
	betaGot, _ := ratToInt64Even(f.Beta)
	if abs64(betaGot-truth.BetaNS) > 500_000 {
		t.Fatalf("beta=%d truth=%d exceeds 500000ns", betaGot, truth.BetaNS)
	}
	if d := f.PPM - truth.DriftPPM; d > 5 || d < -5 {
		t.Fatalf("ppm=%v truth=%v exceeds 5ppm", f.PPM, truth.DriftPPM)
	}
	if f.ResP95NS > 1_000_000 {
		t.Fatalf("p95=%d exceeds 1000000ns", f.ResP95NS)
	}
	if summary.CorrectedCount != truth.VideoCount+truth.AudioCount {
		t.Fatalf("corrected=%d want %d", summary.CorrectedCount, truth.VideoCount+truth.AudioCount)
	}

	raw, _ := os.ReadFile(jp)
	var rows []jsonRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != summary.CorrectedCount {
		t.Fatalf("json rows %d != count %d", len(rows), summary.CorrectedCount)
	}
	if fi, err := os.Stat(cp); err != nil || fi.Size() == 0 {
		t.Fatalf("csv output missing or empty: %v", err)
	}
}

func TestAnalyzeMissingTrack(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "only_video.jsonl")
	if err := os.WriteFile(in, []byte(
		`{"track":"video","epoch":0,"seq":0,"pts":0,"num":1,"den":1000000,"bits":32}`+"\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Analyze(AnalyzeOptions{InPath: in})
	if !errors.Is(err, ErrInput) {
		t.Fatalf("missing track -> ErrInput, got %v", err)
	}
}

func TestAnalyzeDuplicateSyncID(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "dup.jsonl")
	lines := ""
	for i := 0; i < 8; i++ {
		lines += `{"track":"video","epoch":0,"seq":` + strconv.Itoa(i) +
			`,"pts":` + strconv.Itoa(i*1000) + `,"num":1,"den":1000000,"bits":32,"sync_id":"dup"}` + "\n"
		lines += `{"track":"audio","epoch":0,"seq":` + strconv.Itoa(i) +
			`,"pts":` + strconv.Itoa(i*1000) + `,"num":1,"den":1000000,"bits":32,"sync_id":"dup"}` + "\n"
	}
	if err := os.WriteFile(in, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Analyze(AnalyzeOptions{InPath: in})
	var le *LineError
	if !errors.As(err, &le) {
		t.Fatalf("duplicate sync id must report line number, got %v", err)
	}
}

func TestAnalyzeUnreliableExitPath(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "short.jsonl")
	// 锚点充足但跨度不足 60 秒，应在拟合阶段拒绝（退出码 3 语义）。
	lines := ""
	for i := 0; i < 10; i++ {
		lines += `{"track":"video","epoch":0,"seq":` + strconv.Itoa(i) +
			`,"pts":` + strconv.Itoa(i*1000) + `,"num":1,"den":1000000,"bits":32,"sync_id":"s` + strconv.Itoa(i) + `"}` + "\n"
		lines += `{"track":"audio","epoch":0,"seq":` + strconv.Itoa(i) +
			`,"pts":` + strconv.Itoa(i*1000) + `,"num":1,"den":1000000,"bits":32,"sync_id":"s` + strconv.Itoa(i) + `"}` + "\n"
	}
	if err := os.WriteFile(in, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Analyze(AnalyzeOptions{InPath: in})
	if !errors.Is(err, ErrUnreliable) {
		t.Fatalf("short span -> ErrUnreliable, got %v", err)
	}
	// 失败时不得留下任何输出文件。
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("failure left artifacts: %v", entries)
	}
}

func TestAnalyzeNoHalfFinishedOutputsOnBadInput(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "bad.jsonl")
	jp := filepath.Join(dir, "out.json")
	cp := filepath.Join(dir, "out.csv")
	_ = os.WriteFile(in, []byte("{broken json\n"), 0o644)
	_, err := Analyze(AnalyzeOptions{InPath: in, JSONOut: jp, CSVOut: cp})
	if err == nil {
		t.Fatal("expected error")
	}
	if _, err := os.Stat(jp); !os.IsNotExist(err) {
		t.Fatal("json must not exist after failure")
	}
	if _, err := os.Stat(cp); !os.IsNotExist(err) {
		t.Fatal("csv must not exist after failure")
	}
}

// jsonlLine 序列化单条 RawRecord 为 JSONL 文本。
func jsonlLine(t *testing.T, rr RawRecord) string {
	t.Helper()
	b, err := json.Marshal(rr)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// TestAnalyzeCounterWrapEndToEnd 手工构造跨越 32 位边界的视频计数器，
// 验证解绕→配对→拟合→校正全链路在真实回绕下仍恢复真值。
func TestAnalyzeCounterWrapEndToEnd(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "wrap.jsonl")
	const (
		period32u = int64(1) << 32 // 视频 µs 计数器周期
		alpha     = 1.00024
		betaNS    = 80_000_000.0
	)
	var lines string
	// 每轨首条为 pts=0 的非锚点记录（承担“首点归零”），锚点自其后开始，
	// β 才不会被归零动作吞掉。
	lines += jsonlLine(t, RawRecord{Track: "video", Epoch: 0, Seq: 0, PTS: 0, Num: 1, Den: 1_000_000, Bits: 32})
	lines += jsonlLine(t, RawRecord{Track: "audio", Epoch: 0, Seq: 0, PTS: 0, Num: 1, Den: 1_000_000, Bits: 32})
	// 视频锚点绝对读数（µs）：首点 1s（与零记录差 1s，合法），步长 600s、
	// 共 9 个，跨度 4800s 跨过 2^32µs（≈4294.97s）回绕点；相邻增量远小于
	// 半周期，回绕可被解绕器还原。音频侧 4801s 内 µs 计数器同样会回绕，
	// 故音频改用 33 位计数器（周期 ≈2.48 小时），4801s 内不回绕。
	const (
		startU = int64(1_000_000)
		stepU  = int64(600_000_000)
	)
	wrapped := false
	var prevRaw int64
	for k := int64(0); k < 9; k++ {
		vTick := startU + k*stepU // 视频绝对 µs
		rawV := vTick & (period32u - 1)
		if k > 0 && rawV < prevRaw {
			wrapped = true
		}
		prevRaw = rawV
		// 解绕归零读数（纳秒）：首记录在 0µs，首锚点绝对读数 startU，
		// 故归零值 = (vTick-0)µs 经解绕还原；跨度内回绕由解绕器处理。
		vNS := vTick * 1000
		id := "w" + strconv.Itoa(int(k))
		lines += jsonlLine(t, RawRecord{
			Track: "video", Epoch: 0, Seq: k + 1, PTS: rawV,
			Num: 1, Den: 1_000_000, Bits: 32, Sync: &id,
		})
		// 音频 µs 读数为模型 a=αv+β；33 位计数器在该跨度内不回绕。
		// 音频零记录在 0µs，锚点归零读数即 aTick 本身。
		aTick := int64(mathRound((betaNS + alpha*float64(vNS)) / 1000.0))
		lines += jsonlLine(t, RawRecord{
			Track: "audio", Epoch: 0, Seq: k + 1, PTS: aTick,
			Num: 1, Den: 1_000_000, Bits: 33, Sync: &id,
		})
	}
	if !wrapped {
		t.Fatal("test setup failed: video counter did not wrap")
	}
	if err := os.WriteFile(in, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	summary, err := Analyze(AnalyzeOptions{InPath: in, Target: Timebase{1, 1e9}})
	if err != nil {
		t.Fatalf("analyze across wrap: %v", err)
	}
	f := summary.Fits[0]
	betaGot, _ := ratToInt64Even(f.Beta)
	if abs64(betaGot-int64(betaNS)) > 500_000 {
		t.Fatalf("beta across wrap=%d want ~%d", betaGot, int64(betaNS))
	}
	if d := f.PPM - 240; d > 5 || d < -5 {
		t.Fatalf("ppm across wrap=%v want ~240", f.PPM)
	}
	if summary.CorrectedCount != 20 {
		t.Fatalf("count=%d want 20", summary.CorrectedCount)
	}
}

// TestAnalyzeEpochRestart 两个 epoch（设备重启）独立归零、独立拟合；
// 正 β 段首条音频记录校正后为负时间，验证负时间原样保留。
func TestAnalyzeEpochRestart(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "restart.jsonl")
	// 两个 epoch 各自独立归零、β 不同；正 β 段首条音频记录校正后为负时间，
	// 借此同时验证负时间保留（负原始计数器值本就被输入校验禁止）。
	betas := []int64{80_000_000, 120_000_000}
	var lines string
	for ep := int64(0); ep < 2; ep++ {
		// 每段首条为 pts=0 非锚点记录，承担该 epoch 的首点归零。
		lines += jsonlLine(t, RawRecord{Track: "video", Epoch: ep, Seq: 0, PTS: 0, Num: 1, Den: 1_000_000, Bits: 32})
		lines += jsonlLine(t, RawRecord{Track: "audio", Epoch: ep, Seq: 0, PTS: 0, Num: 1, Den: 1_000_000, Bits: 32})
		for k := int64(0); k < 8; k++ {
			vNS := k * 10_000_000_000
			id := "e" + strconv.Itoa(int(ep)) + "-" + strconv.Itoa(int(k))
			lines += jsonlLine(t, RawRecord{
				Track: "video", Epoch: ep, Seq: k + 1, PTS: vNS / 1000,
				Num: 1, Den: 1_000_000, Bits: 32, Sync: &id,
			})
			aNS := int64(mathRound(float64(betas[ep]) + 1.0001*float64(vNS)))
			lines += jsonlLine(t, RawRecord{
				Track: "audio", Epoch: ep, Seq: k + 1, PTS: aNS / 1000,
				Num: 1, Den: 1_000_000, Bits: 32, Sync: &id,
			})
		}
	}
	if err := os.WriteFile(in, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	jp := filepath.Join(dir, "o.json")
	summary, err := Analyze(AnalyzeOptions{InPath: in, JSONOut: jp, Target: Timebase{1, 1e9}})
	if err != nil {
		t.Fatalf("analyze restart: %v", err)
	}
	if len(summary.Epochs) != 2 {
		t.Fatalf("want 2 segments, got %d", len(summary.Epochs))
	}
	for i, ep := range summary.Epochs {
		betaGot, _ := ratToInt64Even(summary.Fits[ep].Beta)
		if abs64(betaGot-betas[i]) > 500_000 {
			t.Fatalf("epoch %d beta=%d want ~%d", ep, betaGot, betas[i])
		}
	}
	raw, _ := os.ReadFile(jp)
	var rows []jsonRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	hasNeg := false
	segments := map[int]bool{}
	for _, r := range rows {
		segments[r.Segment] = true
		if r.CorrectedPTS < 0 {
			hasNeg = true
		}
	}
	if !segments[0] || !segments[1] {
		t.Fatalf("segment numbers missing: %v", segments)
	}
	if !hasNeg {
		t.Fatal("negative corrected timestamp from negative beta must be preserved")
	}
}

func mathRound(v float64) int64 { return int64(math.Round(v)) }

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
