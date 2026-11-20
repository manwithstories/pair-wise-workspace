package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := run(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func TestFixtureThenRepairThenVerify(t *testing.T) {
	dir := t.TempDir()
	code, out, errOut := runCLI(t, "fixture", "--out", dir)
	if code != 0 {
		t.Fatalf("fixture 退出码 %d: %s", code, errOut)
	}
	if !strings.Contains(out, "timeline=") {
		t.Fatalf("fixture 输出缺少路径: %s", out)
	}
	input := filepath.Join(dir, "timeline.mp4")
	code, out, errOut = runCLI(t, "repair", "--input", input, "--report", "json", "--buffer", "8", "--slow-consumer-ms", "1")
	if code != 0 {
		t.Fatalf("repair 退出码 %d: %s", code, errOut)
	}
	for _, want := range []string{"roundtrip_match", "keyframe_hit_rate", "segment_hashes", "peak_buffer_samples", "backpressure_wait_ms", "sha256"} {
		if !strings.Contains(out, want) {
			t.Fatalf("JSON 报告缺少 %q", want)
		}
	}
	code, out, errOut = runCLI(t, "verify", "--input", input)
	if code != 0 || !strings.Contains(out, "verified=true") {
		t.Fatalf("verify 失败 %d: %s %s", code, out, errOut)
	}
}

func TestVerifyCorruptReportsLocation(t *testing.T) {
	dir := t.TempDir()
	if code, _, e := runCLI(t, "fixture", "--out", dir); code != 0 {
		t.Fatalf("fixture: %s", e)
	}
	code, out, errOut := runCLI(t, "verify", "--input", filepath.Join(dir, "corrupt.mp4"))
	if code != 1 {
		t.Fatalf("损坏素材应退出 1，实际 %d", code)
	}
	for _, want := range []string{"error_stage=parse", "source_offset=", "recoverable=false"} {
		if !strings.Contains(out, want) && !strings.Contains(errOut, want) {
			t.Fatalf("输出缺少 %q: %s %s", want, out, errOut)
		}
	}
}

func TestExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"无参数", nil, 2},
		{"未知子命令", []string{"nope"}, 2},
		{"缺 input", []string{"repair"}, 2},
		{"非法报告格式", []string{"repair", "--input", "x", "--report", "yaml"}, 2},
		{"缓冲非法", []string{"repair", "--input", "x", "--buffer", "0"}, 2},
		{"非法策略", []string{"repair", "--input", "x", "--policy", "zzz"}, 2},
		{"非法模式", []string{"fixture", "--pattern", "zzz"}, 2},
		{"输入不存在", []string{"repair", "--input", "/nonexistent/x.mp4"}, 3},
	}
	for _, c := range cases {
		code, _, _ := runCLI(t, c.args...)
		if code != c.want {
			t.Fatalf("%s: 退出码 %d，期望 %d", c.name, code, c.want)
		}
	}
}

func TestHelp(t *testing.T) {
	code, out, _ := runCLI(t, "help")
	if code != 0 || !strings.Contains(out, "退出码") {
		t.Fatalf("help 输出异常: %d %s", code, out)
	}
}

func TestRepairInvalidMP4IsArtifactFailure(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "x.mp4")
	os.WriteFile(bad, []byte{0, 0, 0, 3, 'f', 't', 'y', 'p'}, 0o644)
	code, _, errOut := runCLI(t, "repair", "--input", bad)
	if code != 1 {
		t.Fatalf("非法素材应退出 1，实际 %d: %s", code, errOut)
	}
	if !strings.Contains(errOut, "error_stage=parse") {
		t.Fatalf("应输出阶段定位: %s", errOut)
	}
}
