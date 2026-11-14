package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tsprobe/internal/psi"
	"tsprobe/internal/slicer"
)

func chdirRepoRoot(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(cwd, "..", ".."))
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	return root
}

func invoke(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func tsPacket(pid uint16, pusi bool, payload []byte) []byte {
	packet := make([]byte, 188)
	for i := range packet {
		packet[i] = 0xff
	}
	packet[0] = 0x47
	packet[1] = byte(pid >> 8)
	if pusi {
		packet[1] |= 0x40
	}
	packet[2] = byte(pid)
	packet[3] = 0x10
	copy(packet[4:], payload)
	return packet
}

func TestCLIExitCodesAndEndToEnd(t *testing.T) {
	root := chdirRepoRoot(t)
	if code, _, _ := invoke(); code != exitUsage {
		t.Fatalf("no args exit=%d", code)
	}
	if code, _, _ := invoke("scan", "-in", ""); code != exitUsage {
		t.Fatalf("missing input exit=%d", code)
	}
	if code, _, _ := invoke("gen", "-out", filepath.Join(t.TempDir(), "x.ts")); code != exitOutput {
		t.Fatalf("outside output exit=%d", code)
	}

	empty := filepath.Join(t.TempDir(), "empty.ts")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := invoke("scan", "-in", empty); code != exitInput {
		t.Fatalf("empty input exit=%d", code)
	}
	noPAT := filepath.Join(t.TempDir(), "nopat.ts")
	if err := os.WriteFile(noPAT, tsPacket(0x100, false, []byte{1, 2, 3}), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := invoke("scan", "-in", noPAT); code != exitNoTables {
		t.Fatalf("no PAT exit=%d", code)
	}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	input := filepath.Join(root, "testdata", "cli-"+suffix+".ts")
	clipDir := filepath.Join(root, "out", "cli-"+suffix)
	indexPath := filepath.Join(root, "out", "cli-"+suffix+".json")
	t.Cleanup(func() {
		_ = os.Remove(input)
		_ = os.Remove(indexPath)
		_ = os.RemoveAll(clipDir)
	})
	if code, _, stderr := invoke("gen", "-out", input, "-programs", "2", "-duration", "12s",
		"-loss", "7", "-ccjump", "3", "-versions", "4"); code != exitOK {
		t.Fatalf("gen exit=%d: %s", code, stderr)
	}
	code, stdout, stderr := invoke("scan", "-in", input)
	if code != exitOK {
		t.Fatalf("scan exit=%d: %s", code, stderr)
	}
	var summary struct {
		PATVersions  int              `json:"pat_versions"`
		CCJumps      int              `json:"cc_jumps"`
		LostPackets  int              `json:"lost_packets"`
		PacketCounts map[string]int64 `json:"packet_counts"`
	}
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.PATVersions != 4 || summary.CCJumps != 3 || summary.LostPackets != 7 ||
		summary.PacketCounts["256"] != 1180 || summary.PacketCounts["4096"] != 24 {
		t.Fatalf("unexpected scan summary: %+v", summary)
	}

	code, stdout, stderr = invoke("slice", "-in", input, "-out-dir", clipDir,
		"-min-duration", "2s", "-index", indexPath)
	if code != exitOK {
		t.Fatalf("slice exit=%d: %s", code, stderr)
	}
	var index slicer.Index
	if err := json.Unmarshal([]byte(stdout), &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Slices) < 3 || len(index.Slices) > 6 {
		t.Fatalf("slices=%d", len(index.Slices))
	}
	if code, _, stderr = invoke("verify", "-index", indexPath, "-dir", clipDir); code != exitOK {
		t.Fatalf("verify exit=%d: %s", code, stderr)
	}
	index.Slices[0].PacketCount++
	if err := slicer.WriteIndex(indexPath, index.Slices); err != nil {
		t.Fatal(err)
	}
	if code, _, _ = invoke("verify", "-index", indexPath, "-dir", clipDir); code != exitMismatch {
		t.Fatalf("tampered verify exit=%d", code)
	}

	emptyInput := filepath.Join(t.TempDir(), "tables.ts")
	pat, _ := psi.BuildPAT(1, 0, []psi.PATProgram{{Program: 1, PID: 0x1000}})
	pmt, _ := psi.BuildPMT(1, 0x100, 0, []psi.ESInfo{{StreamType: 0x1b, ElementaryPID: 0x100}}, nil)
	var stream []byte
	for _, packet := range psi.PacketizeSection(0, 0, pat) {
		stream = append(stream, packet...)
	}
	for _, packet := range psi.PacketizeSection(0x1000, 0, pmt) {
		stream = append(stream, packet...)
	}
	if err := os.WriteFile(emptyInput, stream, 0o644); err != nil {
		t.Fatal(err)
	}
	emptyClipDir := filepath.Join(root, "testdata", "empty-clips-"+suffix)
	emptyIndex := filepath.Join(root, "testdata", "empty-index-"+suffix+".json")
	t.Cleanup(func() { _ = os.RemoveAll(emptyClipDir); _ = os.Remove(emptyIndex) })
	if code, _, _ = invoke("slice", "-in", emptyInput, "-out-dir", emptyClipDir,
		"-min-duration", "2s", "-index", emptyIndex); code != exitNoSlices {
		t.Fatalf("no keyframe slice exit=%d", code)
	}
}
