package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"relay/internal/fixture"
	"relay/internal/media"
	"relay/internal/pipeline"
)

func TestParseBuffer(t *testing.T) {
	cases := map[string]int64{
		"64MiB": 64 << 20,
		"1024":  1024,
		"2GiB":  2 << 30,
		"500MB": 500 * 1000 * 1000,
	}
	for in, want := range cases {
		got, err := parseBuffer(in)
		if err != nil || got != want {
			t.Fatalf("parseBuffer(%q)=%d,%v want %d", in, got, err, want)
		}
	}
	if _, err := parseBuffer("notasize"); err == nil {
		t.Fatal("bad size must error")
	}
}

func TestExitCodeMapping(t *testing.T) {
	if ExitCode(nil) != media.ExitOK {
		t.Fatal("nil -> 0")
	}
	if ExitCode(pipeline.ErrInterrupted) != media.ExitInterrupt {
		t.Fatal("interrupt -> 5")
	}
	if ExitCode(media.NewFault(media.StageCRC, 0, 0, 0, "x")) != media.ExitMediaCorruption {
		t.Fatal("crc -> 2")
	}
	if ExitCode(media.NewFault(media.StageCheckpoint, 0, 0, 0, "x")) != media.ExitCheckpoint {
		t.Fatal("checkpoint -> 4")
	}
	if ExitCode(media.NewFault(media.StageConfig, 0, 0, 0, "x")) != media.ExitUsage {
		t.Fatal("config -> 3")
	}
}

// run executes the root command with args and captures stdout.
func run(t *testing.T, args ...string) (int, map[string]any) {
	t.Helper()
	root := NewRootCommand()
	root.SetArgs(args)
	var buf bytes.Buffer
	prev := outWriter
	outWriter = &buf
	t.Cleanup(func() { outWriter = prev })
	err := root.Execute()
	out := map[string]any{}
	if buf.Len() > 0 {
		_ = json.Unmarshal(buf.Bytes(), &out)
	}
	return ExitCode(err), out
}

func TestScanBadCRCReturnsCode2AndCoordinates(t *testing.T) {
	dir := t.TempDir()
	fx, err := fixture.Generate(fixture.Plan{Dir: dir, Frames: 40, Faults: []string{"crc"}})
	if err != nil {
		t.Fatal(err)
	}
	code, out := run(t, "scan", "--in", fx.BadCRC)
	if code != media.ExitMediaCorruption {
		t.Fatalf("scan exit = %d, want 2", code)
	}
	if out["stage"] != media.StageCRC {
		t.Fatalf("stage = %v", out["stage"])
	}
	// offset_error 必须为 0（精确偏移），且带 record/byte_offset 字段。
	if num, _ := out["offset_error"].(float64); num != 0 {
		t.Fatalf("offset_error = %v, want 0", out["offset_error"])
	}
	if _, ok := out["byte_offset"]; !ok {
		t.Fatal("missing byte_offset")
	}
	if _, ok := out["record"]; !ok {
		t.Fatal("missing record")
	}
}

func TestScanCleanReturnsCode0(t *testing.T) {
	dir := t.TempDir()
	fx, _ := fixture.Generate(fixture.Plan{Dir: dir, Frames: 40})
	code, out := run(t, "scan", "--in", fx.Live)
	if code != media.ExitOK || out["status"] != "ok" {
		t.Fatalf("clean scan: code=%d out=%v", code, out)
	}
}

func TestFixtureAndDigestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	code, _ := run(t, "fixture", "--dir", dir, "--frames", "200",
		"--faults", "crc,truncated,late,duplicate,slow,interrupt")
	if code != media.ExitOK {
		t.Fatalf("fixture code=%d", code)
	}
	live := filepath.Join(dir, "live.mfs")
	inDigest := filepath.Join(dir, "in.payload")
	if code, _ = run(t, "digest", "--in", live, "--out", inDigest); code != media.ExitOK {
		t.Fatalf("digest in code=%d", code)
	}
	if _, err := os.Stat(inDigest); err != nil {
		t.Fatalf("digest file not written: %v", err)
	}
}

func TestMissingFlagIsUsageError(t *testing.T) {
	root := NewRootCommand()
	root.SetArgs([]string{"scan"}) // 缺少必填 --in
	err := root.Execute()
	if ExitCode(err) != media.ExitUsage {
		t.Fatalf("missing flag code = %d, want 3", ExitCode(err))
	}
}
