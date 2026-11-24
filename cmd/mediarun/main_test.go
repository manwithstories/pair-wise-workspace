package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLI(args ...string) (int, string, string) {
	var out, errBuf bytes.Buffer
	code := run(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func TestUsageErrors(t *testing.T) {
	if code, _, _ := runCLI(); code != 2 {
		t.Fatalf("no args: %d", code)
	}
	if code, _, _ := runCLI("bogus"); code != 2 {
		t.Fatalf("unknown command: %d", code)
	}
	if code, out, _ := runCLI("help"); code != 0 || !strings.Contains(out, "usage") {
		t.Fatalf("help: %d %q", code, out)
	}
}

func TestDemoBadFlagsExitTwo(t *testing.T) {
	cases := [][]string{
		{"demo", "--packet-loss", "nope"},
		{"demo", "--bandwidth", "2,abc"},
		{"demo", "--bandwidth", ""},
		{"demo", "--bandwidth", "0"},
		{"demo", "--seed", "42", "extra"},
		{"demo", "--bogus-flag"},
		{"verify", "--bogus-flag"},
	}
	for _, args := range cases {
		if code, _, _ := runCLI(args...); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}

func TestDemoNormalPath(t *testing.T) {
	dir := t.TempDir()
	code, out, errOut := runCLI("demo", "--seed", "42", "--packet-loss", "burst", "--bandwidth", "2,8,1", "--out", dir)
	if code != 0 {
		t.Fatalf("demo exit %d: %s", code, errOut)
	}
	for _, want := range []string{"roundtrip_rate=1.000", "keyframe_hit_rate=1.000", "segments=4", "segment_hashes[0]="} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "detected_gaps=0") {
		t.Fatalf("burst scenario reported no gaps:\n%s", out)
	}
	if !strings.Contains(out, "rollback_count=1") || !strings.Contains(out, "recovered=1") {
		t.Fatalf("missing recovery lines:\n%s", out)
	}
	// The independent verify subcommand must agree.
	code, vout, verr := runCLI("verify", "--dir", dir)
	if code != 0 {
		t.Fatalf("verify exit %d: %s", code, verr)
	}
	for _, want := range []string{"recovered=1", "committed_hash_match=1"} {
		if !strings.Contains(vout, want) {
			t.Fatalf("verify missing %q:\n%s", want, vout)
		}
	}
	if !strings.Contains(vout, "status=ok") {
		t.Fatalf("verify did not report ok:\n%s", vout)
	}
}

func TestDemoAllScenarios(t *testing.T) {
	for _, scenario := range []string{"clean", "reorder", "duplicate", "burst", "wrap"} {
		dir := t.TempDir()
		code, out, errOut := runCLI("demo", "--seed", "7", "--packet-loss", scenario, "--bandwidth", "1,2", "--out", dir)
		if code != 0 {
			t.Fatalf("%s exit %d: %s", scenario, code, errOut)
		}
		if !strings.Contains(out, "roundtrip_rate=1.000") {
			t.Fatalf("%s roundtrip:\n%s", scenario, out)
		}
	}
}

func TestDemoReproducible(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	_, outA, _ := runCLI("demo", "--seed", "99", "--packet-loss", "reorder", "--bandwidth", "4", "--out", dirA)
	_, outB, _ := runCLI("demo", "--seed", "99", "--packet-loss", "reorder", "--bandwidth", "4", "--out", dirB)
	hashesA := grepHashes(outA)
	hashesB := grepHashes(outB)
	if len(hashesA) == 0 || strings.Join(hashesA, ",") != strings.Join(hashesB, ",") {
		t.Fatalf("hashes differ:\n%s\n%s", strings.Join(hashesA, "\n"), strings.Join(hashesB, "\n"))
	}
}

func grepHashes(out string) []string {
	var hashes []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "segment_hashes") {
			hashes = append(hashes, line)
		}
	}
	return hashes
}

func TestVerifyDetectsTamper(t *testing.T) {
	dir := t.TempDir()
	if code, _, errOut := runCLI("demo", "--out", dir); code != 0 {
		t.Fatalf("demo: %s", errOut)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var seg string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "seg-") {
			seg = e.Name()
			break
		}
	}
	if seg == "" {
		t.Fatal("no segment file")
	}
	if err := os.WriteFile(filepath.Join(dir, seg), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runCLI("verify", "--dir", dir)
	if code != 1 {
		t.Fatalf("tampered verify exit %d: %s", code, errOut)
	}
}

func TestVerifyEmptyDirExitTwo(t *testing.T) {
	if code, _, _ := runCLI("verify", "--dir", t.TempDir()); code != 2 {
		t.Fatalf("empty dir exit %d", code)
	}
}

func TestParseBandwidth(t *testing.T) {
	got, err := parseBandwidth(" 2, 8 ,1 ")
	if err != nil || len(got) != 3 || got[1] != 8 {
		t.Fatalf("parse: %v %v", got, err)
	}
	for _, bad := range []string{"", "1,,2", "x", "10001", "-1"} {
		if _, err := parseBandwidth(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
