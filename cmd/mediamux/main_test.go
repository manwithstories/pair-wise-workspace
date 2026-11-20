package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// captureStdout 捕获 fn 的标准输出并返回其退出码。
func captureStdout(t *testing.T, fn func() int) (string, int) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	code := fn()
	_ = w.Close()
	os.Stdout = old
	return <-done, code
}

func demoArgs(extra ...string) []string {
	base := []string{
		"--seed", "17",
		"--target-bitrate", "2400000",
		"--buffer-ms", "180",
		"--memory-mb", "32",
		"--slow-consumer-ms", "1",
		"--duration-ms", "4000",
	}
	return append(base, extra...)
}

func TestDemoAcceptance(t *testing.T) {
	out, code := captureStdout(t, func() int { return runDemo(demoArgs()) })
	if code != exitOK {
		t.Fatalf("exit = %d, out = %s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 6 { // 1 汇总 + 5 夹具
		t.Fatalf("lines = %d: %v", len(lines), lines)
	}
	var sum map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &sum); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		field string
		max   float64
	}{
		{"bitrate_deviation_pct", 5},
		{"sync_error_ms_p95", 40},
		{"manifest_alignment_ms_max", 40}, // 1 帧 @25fps
		{"buffer_peak_bytes", 32 << 20},
	}
	for _, c := range checks {
		v, ok := sum[c.field].(float64)
		if !ok {
			t.Fatalf("missing %s in %v", c.field, sum)
		}
		if v > c.max {
			t.Errorf("%s = %v > %v", c.field, v, c.max)
		}
	}
	for _, f := range []string{"packet_loss", "recovered_packets", "subtitle_valid"} {
		if sum[f].(float64) <= 0 {
			t.Errorf("%s should be > 0: %v", f, sum[f])
		}
	}
	// 夹具：错误偏移可追溯，坏记录后仍有恢复
	for _, line := range lines[1:] {
		var fx map[string]any
		if err := json.Unmarshal([]byte(line), &fx); err != nil {
			t.Fatal(err)
		}
		if fx["error_offset"].(float64) < 0 {
			t.Errorf("fixture %v missing error_offset", fx["fixture"])
		}
		if fx["invalid"].(float64) == 0 {
			t.Errorf("fixture %v should have invalid records", fx["fixture"])
		}
	}
}

func TestDemoDeterministic(t *testing.T) {
	out1, code1 := captureStdout(t, func() int { return runDemo(demoArgs()) })
	out2, code2 := captureStdout(t, func() int { return runDemo(demoArgs()) })
	if code1 != code2 || out1 != out2 {
		t.Fatalf("non-deterministic:\n%s\nvs\n%s", out1, out2)
	}
}

func TestDemoUsageError(t *testing.T) {
	if _, code := captureStdout(t, func() int {
		return runDemo([]string{"--target-bitrate", "-1"})
	}); code != exitUsage {
		t.Fatalf("bad params exit = %d", code)
	}
	if _, code := captureStdout(t, func() int {
		return runDemo([]string{"--no-such-flag"})
	}); code != exitUsage {
		t.Fatalf("unknown flag exit = %d", code)
	}
}

func TestDemoQualityGate(t *testing.T) {
	// 时长截断：丢包占比上升使码率偏差超 5%，且无字幕事件 → 退出码 1
	args := []string{
		"--seed", "17", "--target-bitrate", "2400000",
		"--buffer-ms", "180", "--memory-mb", "32",
		"--slow-consumer-ms", "0", "--duration-ms", "1500",
	}
	out, code := captureStdout(t, func() int { return runDemo(args) })
	if code != exitQuality {
		t.Fatalf("truncated duration exit = %d, out = %s", code, out)
	}
}
