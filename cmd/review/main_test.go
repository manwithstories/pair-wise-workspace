package main

import (
	"path/filepath"
	"testing"

	"review/internal/fault"
	"review/internal/metrics"
)

// 端到端：故障注入下链路仍通过全部验收指标。
func TestPipelineEndToEnd(t *testing.T) {
	opts := options{demo: true, injectLoss: 3, truncateTail: true, dropIndex: 2}
	var last *metrics.Report
	digests := map[string]bool{}
	for i := 0; i < 2; i++ {
		rep, faults, err := runPipeline(filepath.Join(t.TempDir(), "run"), opts)
		if err != nil {
			t.Fatal(err)
		}
		if fault.HasFatal(faults) {
			t.Fatalf("unexpected fatal fault: %v", faults)
		}
		// 截断与索引缺项两类故障须穿过解析→恢复→合成链路
		kinds := map[fault.Kind]bool{}
		for _, f := range faults {
			kinds[f.Kind] = true
		}
		for _, want := range []fault.Kind{fault.KindTruncated, fault.KindMissingIndex, fault.KindCRC} {
			if !kinds[want] {
				t.Fatalf("fault kind %s not observed in chain", want)
			}
		}
		digests[rep.SHA256] = true
		last = rep
	}
	last.RepeatHashEqual = len(digests) == 1
	if !last.Pass() {
		t.Fatalf("report not passing: %+v", last)
	}
	if last.Frames != metrics.WantFrames {
		t.Fatalf("frames=%d", last.Frames)
	}
	if last.StageCommits["parse"] != 1 || last.StageCommits["render"] != 1 {
		t.Fatalf("commits=%v", last.StageCommits)
	}
}

// 丢包超过分组数 → 不可恢复，须报错（退出码 3 路径）。
func TestPipelineUnrecoverableLoss(t *testing.T) {
	opts := options{demo: true, injectLoss: 100000}
	_, _, err := runPipeline(t.TempDir(), opts)
	if err == nil {
		t.Fatal("want unrecoverable error")
	}
}

// 无故障基线同样通过。
func TestPipelineClean(t *testing.T) {
	rep, _, err := runPipeline(t.TempDir(), options{demo: true})
	if err != nil {
		t.Fatal(err)
	}
	rep.RepeatHashEqual = true
	if !rep.Pass() {
		t.Fatalf("clean run not passing: %+v", rep)
	}
	if rep.LossRecoveryPct != 100 || rep.SyncMaxMs > metrics.SyncMaxMsLimit {
		t.Fatalf("report=%+v", rep)
	}
}
