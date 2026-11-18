package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestExitCodes 验证命令层退出码契约：0 成功、2 输入错误、3 不可拟合、4 读写失败。
func TestExitCodes(t *testing.T) {
	dir := t.TempDir()

	// 先准备一份可用样例作为成功路径输入。
	good := filepath.Join(dir, "good.jsonl")
	if _, err := Generate(GenConfig{Seed: 1, Seconds: 120, OutPath: good}); err != nil {
		t.Fatal(err)
	}

	t.Run("success=0", func(t *testing.T) {
		code := run([]string{"analyze", "--in", good,
			"--json", filepath.Join(dir, "o.json"),
			"--csv", filepath.Join(dir, "o.csv")})
		if code != exitOK {
			t.Fatalf("exit=%d want 0", code)
		}
	})

	t.Run("missing_in_flag=2", func(t *testing.T) {
		if code := run([]string{"analyze"}); code != exitInput {
			t.Fatalf("exit=%d want 2", code)
		}
	})

	t.Run("nonexistent_input=4", func(t *testing.T) {
		code := run([]string{"analyze", "--in", filepath.Join(dir, "nope.jsonl")})
		if code != exitIO {
			t.Fatalf("exit=%d want 4", code)
		}
	})

	t.Run("malformed_json=2", func(t *testing.T) {
		bad := filepath.Join(dir, "bad.jsonl")
		if err := os.WriteFile(bad, []byte("{not-json\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if code := run([]string{"analyze", "--in", bad}); code != exitInput {
			t.Fatalf("exit=%d want 2", code)
		}
	})

	t.Run("unreliable_fit=3", func(t *testing.T) {
		// 跨度不足 60 秒的合法两轨数据。
		short := filepath.Join(dir, "short.jsonl")
		var lines string
		for k := 0; k < 10; k++ {
			id := "s" + strconv.Itoa(k)
			lines += jsonlLine(t, RawRecord{Track: "video", Epoch: 0, Seq: int64(k),
				PTS: int64(k * 1000), Num: 1, Den: 1_000_000, Bits: 32, Sync: &id})
			lines += jsonlLine(t, RawRecord{Track: "audio", Epoch: 0, Seq: int64(k),
				PTS: int64(k * 1000), Num: 1, Den: 1_000_000, Bits: 32, Sync: &id})
		}
		if err := os.WriteFile(short, []byte(lines), 0o644); err != nil {
			t.Fatal(err)
		}
		if code := run([]string{"analyze", "--in", short}); code != exitUnreliable {
			t.Fatalf("exit=%d want 3", code)
		}
	})

	t.Run("bad_target_timebase=2", func(t *testing.T) {
		code := run([]string{"analyze", "--in", good, "--tb-num", "0"})
		if code != exitInput {
			t.Fatalf("exit=%d want 2", code)
		}
	})

	t.Run("unwritable_output=4", func(t *testing.T) {
		// 把输出路径指向一个已存在的普通文件所在“目录”的非法子路径。
		blocker := filepath.Join(dir, "afile")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		code := run([]string{"analyze", "--in", good,
			"--json", filepath.Join(blocker, "o.json")})
		if code != exitIO {
			t.Fatalf("exit=%d want 4", code)
		}
	})

	t.Run("generate_bad_args=2", func(t *testing.T) {
		if code := run([]string{"generate", "--seed", "1", "--out", filepath.Join(dir, "x.jsonl")}); code != exitInput {
			t.Fatalf("exit=%d want 2", code)
		}
	})

	t.Run("unknown_command=2", func(t *testing.T) {
		if code := run([]string{"frobnicate"}); code != exitInput {
			t.Fatalf("exit=%d want 2", code)
		}
	})

	t.Run("no_anchor=3", func(t *testing.T) {
		noAnchor := filepath.Join(dir, "noanchor.jsonl")
		lines := jsonlLine(t, RawRecord{Track: "video", Epoch: 0, Seq: 0, PTS: 0, Num: 1, Den: 1e6, Bits: 32}) +
			jsonlLine(t, RawRecord{Track: "audio", Epoch: 0, Seq: 0, PTS: 0, Num: 1, Den: 1e6, Bits: 32})
		if err := os.WriteFile(noAnchor, []byte(lines), 0o644); err != nil {
			t.Fatal(err)
		}
		if code := run([]string{"analyze", "--in", noAnchor}); code != exitUnreliable {
			t.Fatalf("exit=%d want 3", code)
		}
	})
}
