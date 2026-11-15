package timeline

import (
	"math/big"
	"testing"

	"subtool/internal/model"
)

func TestFrameMapping(t *testing.T) {
	fps, err := ParseFPS("24000/1001")
	if err != nil {
		t.Fatal(err)
	}
	// 1001 ms at 23.976fps = exactly 24 frames.
	if got := fps.FrameAt(big.NewRat(1001, 1), RoundFloor); got != 24 {
		t.Errorf("1001ms -> frame %d, want 24", got)
	}
	// Half frame boundary: 1 frame = 41.7083ms; 20.854ms is half.
	half := fps.MsAtFrame(1)
	half.Quo(half, big.NewRat(2, 1))
	if got := fps.FrameAt(half, RoundNearest); got != 1 {
		t.Errorf("half frame nearest -> %d, want 1", got)
	}
	if got := fps.FrameAt(half, RoundFloor); got != 0 {
		t.Errorf("half frame floor -> %d, want 0", got)
	}
	if got := fps.FrameAt(half, RoundCeil); got != 1 {
		t.Errorf("half frame ceil -> %d, want 1", got)
	}
}

func TestSnapEndNotBeforeStart(t *testing.T) {
	fps, _ := ParseFPS("24")
	c := model.Cue{Start: big.NewRat(1000, 1), End: big.NewRat(1010, 1)}
	fps.Snap(&c, RoundNearest)
	if c.EndFrame < c.StartFrame {
		t.Errorf("end %d < start %d", c.EndFrame, c.StartFrame)
	}
	if !c.Subframe {
		t.Error("10ms cue at 24fps should be subframe")
	}
}

func TestDropTimecode(t *testing.T) {
	fps, _ := ParseFPS("30000/1001")
	// Frame 17982 = 00:10:00;00 in 29.97 DF (drops 18 frames by 10min).
	got := DropTimecode(17982, fps)
	if got != "00:10:00;00" {
		t.Errorf("drop tc: got %s", got)
	}
	// Non-drop rate uses colon.
	fps24, _ := ParseFPS("24")
	if got := DropTimecode(24*60, fps24); got != "00:01:00:00" {
		t.Errorf("plain tc: got %s", got)
	}
}

func TestOverlapLayerCoexist(t *testing.T) {
	cues := []model.Cue{
		{ID: "a", Start: big.NewRat(0, 1), End: big.NewRat(1000, 1)},
		{ID: "b", Start: big.NewRat(500, 1), End: big.NewRat(1500, 1)},
		{ID: "c", Start: big.NewRat(1500, 1), End: big.NewRat(2000, 1)}, // touching, not overlap
	}
	out, stats := Resolve(cues, ModeLayer)
	if len(out) != 3 {
		t.Fatalf("layer mode must keep all cues, got %d", len(out))
	}
	if stats.Relayered != 1 {
		t.Errorf("expected 1 relayered cue, got %d", stats.Relayered)
	}
	// Touching cues must not merge or shift layers.
	for _, c := range out {
		if c.ID == "c" && c.Layer != 0 {
			t.Errorf("touching cue moved to layer %d", c.Layer)
		}
	}
}

func TestOverlapTrim(t *testing.T) {
	cues := []model.Cue{
		{ID: "a", Start: big.NewRat(0, 1), End: big.NewRat(1000, 1)},
		{ID: "b", Start: big.NewRat(400, 1), End: big.NewRat(900, 1)},
	}
	out, stats := Resolve(cues, ModeTrim)
	if stats.Trims != 1 {
		t.Fatalf("expected 1 trim, got %d", stats.Trims)
	}
	if stats.TrimmedMs.Num().Int64() != 600 {
		t.Errorf("trimmed ms: got %s", stats.TrimmedMs)
	}
	if len(out) != 2 {
		t.Fatalf("both cues should survive trim, got %+v", out)
	}
	if out[0].End.Num().Int64() != 400 {
		t.Errorf("a should be trimmed to 400ms, got %s", out[0].End)
	}
}
