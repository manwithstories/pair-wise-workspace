package report_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"tsaudit/internal/fixtures"
	"tsaudit/internal/report"
	"tsaudit/internal/slicer"
	"tsaudit/internal/ts"
)

func TestFixtureScanAndSliceEndToEnd(t *testing.T) {
	dir := t.TempDir()
	tsPath := filepath.Join(dir, "sample.ts")
	truthPath := filepath.Join(dir, "sample.truth.json")
	cfg := fixtures.Config{Out: tsPath, Truth: truthPath, Seed: 7, Programs: 2,
		PATVersions: 3, PMTVersions: 4, Drop: 2, CCJump: 1, CRCErrors: 1, DurationMS: 6000}
	truth, err := fixtures.Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r, err := report.ScanFile(tsPath)
	if err != nil {
		t.Fatal(err)
	}
	if r.PacketsTotal != truth.PacketsTotal || r.PAT == nil || r.PAT.VersionChanges != 2 {
		t.Fatalf("unexpected scan: packets=%d/%d pat=%+v", r.PacketsTotal, truth.PacketsTotal, r.PAT)
	}
	if len(r.PMTs) != 2 || r.PMTs[0].VersionChanges != 3 || r.PMTs[1].VersionChanges != 3 {
		t.Fatalf("unexpected PMT versions: %+v", r.PMTs)
	}
	if r.Continuity.LossPackets != 2 || r.Continuity.CCJumps != 1 || r.CRCErrors != 1 {
		t.Fatalf("unexpected faults: continuity=%+v crc=%d", r.Continuity, r.CRCErrors)
	}
	reportPath := filepath.Join(dir, "scan.json")
	if err := r.WriteJSON(reportPath); err != nil {
		t.Fatal(err)
	}
	var decoded report.Report
	b, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &decoded); err != nil || decoded.PacketsTotal != r.PacketsTotal {
		t.Fatalf("report round trip failed: %v %+v", err, decoded)
	}

	idx, err := slicer.Run(slicer.Config{In: tsPath, Program: 1, SegmentMS: 2000,
		OutDir: filepath.Join(dir, "seg"), IndexPath: filepath.Join(dir, "index.json")})
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Segments) < 3 {
		t.Fatalf("segments = %d, want at least 3: %+v", len(idx.Segments), idx.Segments)
	}
	if idx.Segments[1].PMTVersion != 1 || idx.Segments[2].PMTVersion != 2 {
		t.Fatalf("segment PMT snapshots leaked a future table: %+v", idx.Segments)
	}
	for _, seg := range idx.Segments {
		b, err := os.ReadFile(filepath.Join(dir, "seg", seg.File))
		if err != nil {
			t.Fatal(err)
		}
		if len(b) < 6 || b[0] != ts.SyncByte || uint16(b[1]&0x1f)<<8|uint16(b[2]) != 0 || b[5] != 0 {
			t.Fatalf("segment %s does not begin with a PAT packet: %x", seg.File, b[:6])
		}
		if seg.PCRStart < 0 || seg.PIDCount == 0 {
			t.Fatalf("invalid segment index: %+v", seg)
		}
	}
}
