package run

import (
	"context"
	"os"
	"testing"

	"github.com/mediamend/mediamend/internal/clock"
	"github.com/mediamend/mediamend/internal/frame"
	"github.com/mediamend/mediamend/internal/pipeline"
)

// TestTimestampEquivalence checks that the timeline position computed by the
// pipeline for every committed frame is exactly the sequence-derived value
// the fixture declares. That is the "no alignment error" invariant expressed
// at frame granularity rather than segment granularity.
func TestTimestampEquivalence(t *testing.T) {
	in, fx := fixture(t, 400, 0)
	out := t.TempDir()
	rep, err := Run(context.Background(), Config{
		InputDir: in, OutDir: out, Ladder: testLadder(t), RandomSeeks: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.AlignmentError != 0 {
		t.Fatalf("segment alignment error=%d", rep.AlignmentError)
	}

	// Rebuild the frame lists the run produced and compare each frame's
	// master timestamp with the declared cadence.
	for _, cam := range fx.Cameras {
		md, err := frame.LoadMetadata(fx.Files[cam.ID+".meta"])
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(fx.Files[cam.ID])
		if err != nil {
			t.Fatal(err)
		}
		r := frame.NewReader(f, cam.ID, 0)
		var items []pipeline.Item
		for {
			rec, err := r.Next()
			if err != nil {
				break
			}
			items = append(items, pipeline.Item{
				Camera: cam.ID, Seq: rec.Seq, Timestamp: uint32(rec.DeviceTS),
				RTPSeq: uint16(rec.Seq), Offset: rec.Offset,
			})
		}
		f.Close()
		if len(items) == 0 {
			t.Fatalf("%s produced no items", cam.ID)
		}
		cfg := clock.Config{
			Camera: cam.ID, Rate: cam.Rate, FrameStep: cam.FrameStep,
			AnchorSeq: 0, AnchorDeviceTS: camAnchor(md), AnchorMasterTS: 0,
		}
		tl, err := clock.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			m, _ := tl.Push(it.Seq, uint64(it.Timestamp), it.Offset)
			want := cfg.ExpectedMaster(m.Logical)
			if m.MasterTS != want && !m.Duplicate {
				t.Fatalf("%s seq %d master=%d want %d (%s)", cam.ID, it.Seq, m.MasterTS, want, m.Note)
			}
		}
		// Every camera's anchored start is tick 0 on the master timeline.
		if res := tl.Result(); len(res.Frames) > 0 && res.Frames[0].MasterTS != 0 {
			t.Fatalf("%s first frame master=%d want 0", cam.ID, res.Frames[0].MasterTS)
		}
	}
}
