package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

func pipe() (*os.File, *os.File, error) { return os.Pipe() }

func readAll(r *os.File) string {
	b, _ := io.ReadAll(r)
	r.Close()
	return string(b)
}

// runDemo 捕获 demo 的 JSON 行输出。
func runDemo(t *testing.T, args ...string) (int, []map[string]any) {
	t.Helper()
	old := stdout
	r, w, err := pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout = w
	code := dispatch(append([]string{"demo"}, args...))
	w.Close()
	stdout = old
	out := readAll(r)
	var lines []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(out), "\n") {
		if ln == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("bad JSON line %q: %v", ln, err)
		}
		lines = append(lines, m)
	}
	return code, lines
}

func TestDemoExitAndFields(t *testing.T) {
	code, lines := runDemo(t,
		"--seed", "42", "--target-kbps", "256", "--rate", "48000",
		"--channels", "2", "--loudness", "-16", "--loss-burst", "4",
		"--bandwidth-kbps", "900")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if len(lines) != 3 {
		t.Fatalf("lines=%d", len(lines))
	}
	normal := lines[0]
	// 必填字段
	for _, k := range []string{"target_kbps", "actual_kbps", "bitrate_error_pct",
		"input_lufs", "output_lufs", "peak_dbfs", "gap_packets",
		"duplicate_packets", "reordered_packets", "manifest_error_ms",
		"index_digest_match", "recovery_action"} {
		if _, ok := normal[k]; !ok {
			t.Fatalf("missing field %s", k)
		}
	}
	if normal["target_kbps"].(float64) != 256 {
		t.Fatalf("target=%v", normal["target_kbps"])
	}
	if d := normal["bitrate_error_pct"].(float64); d > 10 || d < -10 {
		t.Fatalf("bitrate error=%v", d)
	}
	if normal["manifest_error_ms"].(float64) > 20 {
		t.Fatalf("manifest error=%v", normal["manifest_error_ms"])
	}
	if normal["gap_packets"].(float64) != 4 {
		t.Fatalf("gap=%v", normal["gap_packets"])
	}
	if normal["index_digest_match"] != true {
		t.Fatal("digest mismatch")
	}
	// 损坏样例：可定位错误
	corrupt := lines[1]
	if corrupt["error"] == nil || !strings.Contains(corrupt["error"].(string), "seg#") {
		t.Fatalf("corrupt line=%v", corrupt)
	}
	// 恢复样例：摘要校验 + 续跑边界
	resumed := lines[2]
	if resumed["recovery_action"] != "resumed" || resumed["resumed_from_segment"].(float64) != 2 {
		t.Fatalf("resumed line=%v", resumed)
	}
}

func TestDemoRepeatConsistency(t *testing.T) {
	_, l1 := runDemo(t, "--seed", "42", "--loss-burst", "4")
	_, l2 := runDemo(t, "--seed", "42", "--loss-burst", "4")
	if len(l1) != len(l2) {
		t.Fatal("line count differs")
	}
	for i := range l1 {
		a, _ := json.Marshal(l1[i])
		b, _ := json.Marshal(l2[i])
		if string(a) != string(b) {
			t.Fatalf("run differs at line %d:\n%s\n%s", i, a, b)
		}
	}
}

func TestExitCodes(t *testing.T) {
	if code := dispatch([]string{"run", "--manifest", "/nonexistent.mpd"}); code != 2 {
		t.Fatalf("run bad manifest exit=%d", code)
	}
	if code := dispatch([]string{"bogus"}); code != 2 {
		t.Fatalf("bogus exit=%d", code)
	}
	if code := dispatch([]string{}); code != 2 {
		t.Fatalf("empty exit=%d", code)
	}
}

func TestQualityGateExit3(t *testing.T) {
	// 目标码率低于媒体码率（无法填充达到）→ 偏差超 10% → 退出码 3
	old := stdout
	r, w, _ := pipe()
	stdout = w
	code := dispatch([]string{"demo", "--seed", "42", "--target-kbps", "100"})
	w.Close()
	stdout = old
	readAll(r)
	if code != 3 {
		t.Fatalf("exit=%d want 3", code)
	}
}
