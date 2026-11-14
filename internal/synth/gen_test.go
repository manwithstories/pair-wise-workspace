package synth_test

import (
	"os"
	"path/filepath"
	"testing"

	"tsaudit/internal/scan"
	"tsaudit/internal/synth"
)

func TestGeneratedStreamMatchesAcceptanceShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.ts")
	cfg := synth.Config{Programs: 3, Packets: 400000, Bumps: 5, LossBursts: 3, Dups: 12, Seed: 7}
	if err := synthesize(path, cfg); err != nil {
		t.Fatal(err)
	}
	rep, err := scan.File(path, scan.Options{PMTTimeout: 2000, MinSegmentBytes: 65536})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Packets != 400000 || rep.Bytes != 400000*188 {
		t.Fatalf("packets/bytes = %d/%d", rep.Packets, rep.Bytes)
	}
	if rep.PATVersionChanges != 5 {
		t.Fatalf("pat_version_changes = %d", rep.PATVersionChanges)
	}
	if rep.PMTVersionChanges[1] != 4 || rep.PMTVersionChanges[2] != 3 || rep.PMTVersionChanges[3] != 4 {
		t.Fatalf("pmt_version_changes = %+v", rep.PMTVersionChanges)
	}
	if len(rep.PIDMap) != 14 {
		t.Fatalf("pid_map entries = %d", len(rep.PIDMap))
	}
	if rep.ESPacketsTotal < 350000 || rep.ESPacketsTotal > 365000 {
		t.Fatalf("es_packets_total = %d", rep.ESPacketsTotal)
	}
	if rep.CCJumps != 7 || rep.CCDuplicates != 12 || rep.LostPackets != 15 {
		t.Fatalf("cc jumps/dups/lost = %d/%d/%d", rep.CCJumps, rep.CCDuplicates, rep.LostPackets)
	}
	if rep.Segments != 9 {
		t.Fatalf("segments = %d", rep.Segments)
	}
	for _, entry := range rep.PIDMap {
		if entry.Role == "ES" && entry.PacketCount == 0 {
			t.Fatalf("ES PID 0x%04x has no packet count", entry.PID)
		}
	}
}

func synthesize(path string, cfg synth.Config) error {
	f, err := create(path)
	if err != nil {
		return err
	}
	_, genErr := synth.Generate(f, cfg)
	closeErr := f.Close()
	if genErr != nil {
		return genErr
	}
	return closeErr
}

func create(path string) (*os.File, error) { return os.Create(path) }
