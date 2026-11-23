package pipeline

import (
	"path/filepath"
	"testing"

	"mediarun/internal/txn"
)

// TestWALRecordsEveryStage proves the log holds the input hash, stage cursor
// and temporary artefact for every stage, which is what makes a resumed run
// auditable.
func TestWALRecordsEveryStage(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Seed: 9, Segments: 2, SegmentDurNS: 2_000_000_000,
		PacketRate: 50, WorkDir: dir, Faults: Faults{Drop: true}}
	if _, err := Run(cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	recs, err := txn.ReplayFile(filepath.Join(dir, "wal.log"))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	wantStages := []string{"manifest", "timeline", "rtp", "jitter", "sync", "index", "final"}
	for _, stage := range wantStages {
		var begin, checkpoint bool
		for _, r := range recs {
			switch {
			case r.Type == txn.RecStageBegin && r.Stage == stage:
				begin = true
				if r.InputHash == "" || r.Cursor == "" || r.Temp == "" {
					t.Fatalf("stage %s begin record incomplete: %+v", stage, r)
				}
			case r.Type == txn.RecCheckpoint && r.Stage == stage:
				checkpoint = true
				if r.OutputHash == "" || r.Cursor == "" || r.Temp == "" {
					t.Fatalf("stage %s checkpoint incomplete: %+v", stage, r)
				}
			}
		}
		if !begin || !checkpoint {
			t.Fatalf("stage %s missing begin/checkpoint records", stage)
		}
	}
}
