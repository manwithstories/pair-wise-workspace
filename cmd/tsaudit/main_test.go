package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"tsaudit/internal/report"
)

func TestCommandExitCodes(t *testing.T) {
	dir := t.TempDir()
	tsPath := filepath.Join(dir, "a.ts")
	truthPath := filepath.Join(dir, "a.truth.json")
	scanPath := filepath.Join(dir, "scan.json")
	var out, errOut bytes.Buffer
	code := run([]string{"fixtures", "gen", "--out", tsPath, "--truth", truthPath,
		"--seed", "9", "--programs", "2", "--pat-versions", "1", "--pmt-versions", "1",
		"--duration-ms", "2000", "--drop", "1", "--cc-jump", "1", "--crc-errors", "0"}, &out, &errOut)
	if code != exitOK {
		t.Fatalf("fixtures exit=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	code = run([]string{"scan", "--in", tsPath, "--json", "--out", scanPath}, &out, &errOut)
	if code != exitOK {
		t.Fatalf("scan exit=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	if code = run([]string{"scan", "--in", tsPath, "--strict"}, &out, &errOut); code != exitStrict {
		t.Fatalf("strict exit=%d, want %d", code, exitStrict)
	}
	out.Reset()
	if code = run([]string{"verify", "--truth", truthPath, "--report", scanPath}, &out, &errOut); code != exitOK {
		t.Fatalf("verify exit=%d out=%s stderr=%s", code, out.String(), errOut.String())
	}
	var r report.Report
	b, err := os.ReadFile(scanPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	r.PacketsTotal++
	if err := r.WriteJSON(scanPath); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code = run([]string{"verify", "--truth", truthPath, "--report", scanPath}, &out, &errOut); code != exitVerify {
		t.Fatalf("mismatch exit=%d, want %d", code, exitVerify)
	}
	if out.String() == "" {
		t.Fatal("mismatch must print the first differing field")
	}
	if code = run([]string{"verify", "--truth", filepath.Join(dir, "missing"), "--report", scanPath}, &out, &errOut); code != exitUsageIO {
		t.Fatalf("IO error exit=%d, want %d", code, exitUsageIO)
	}
}
