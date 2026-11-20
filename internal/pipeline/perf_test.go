package pipeline

import (
	"testing"
	"time"

	"github.com/mediamend/mediamend/internal/fixture"
)

func TestPerfTenThousandFrames(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	cfg := buildConfig(t, t.TempDir(), fixture.DefaultFramesPerTrack, 97)
	cfg.RandomSeeks = 200
	start := time.Now()
	res, err := Run(cfg)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if res.FramesTotal < 9000 {
		t.Fatalf("frames_total=%d", res.FramesTotal)
	}
	if res.LocationPrecision != 1.0 {
		t.Fatalf("precision=%f located=%d", res.LocationPrecision, res.CorruptLocated)
	}
	if res.SeekChecked != 200*len(cfg.Project.Tracks) {
		t.Fatalf("seek=%d", res.SeekChecked)
	}
	if elapsed > 60*time.Second {
		t.Fatalf("took %v > 60s", elapsed)
	}
	t.Logf("frames=%d elapsed=%v output=%s", res.FramesTotal, elapsed, res.OutputSHA256)
}
