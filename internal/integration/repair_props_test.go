package integration

import (
	"testing"

	"mediacheck/internal/fixture"
	"mediacheck/internal/probe"
	"mediacheck/internal/repair"
)

// TestRecoveredPrefixContiguous asserts recovery never accepts a sample after
// it has skipped one: the accepted indices form a prefix followed by a suffix
// that starts at a keyframe, and that keyframe is exactly the resume point.
func TestRecoveredPrefixContiguous(t *testing.T) {
	for _, kind := range []fixture.Kind{fixture.Corrupt, fixture.Truncated} {
		dir := t.TempDir()
		gen, err := fixture.Generate(dir, fixture.DefaultSpec(kind))
		if err != nil {
			t.Fatal(err)
		}
		rcfg := repair.DefaultConfig()
		rcfg.Recover = true
		pf, res, err := repair.New(rcfg, probe.DefaultConfig()).Analyze(gen.Path)
		if err != nil {
			t.Fatal(err)
		}
		for _, tr := range res.Tracks {
			if tr.Status != repair.StatusRecovered {
				continue
			}
			pt := probeTrackByID(pf, tr.TrackID)
			if pt == nil {
				t.Fatal("missing probe track")
			}
			var keyAt = map[int64]bool{}
			cur := pt.Cursor()
			for {
				row, ok := cur.Next()
				if !ok {
					break
				}
				if row.Keyframe {
					keyAt[row.DTSMS] = true
				}
			}
			for _, d := range tr.Damages {
				if d.Resumed && !keyAt[d.ResumeMS] {
					t.Fatalf("%s track %d resumed at %d ms which is not a keyframe", kind, tr.TrackID, d.ResumeMS)
				}
				if !d.Resumed && !d.Tail {
					t.Fatalf("%s track %d damage neither resumed nor tail", kind, tr.TrackID)
				}
			}
		}
	}
}

// TestUnrecoverableThenUsable checks a fully damaged track leaves the other
// track usable and the overall status partial.
func TestUnrecoverableThenUsable(t *testing.T) {
	dir := t.TempDir()
	gen, err := fixture.Generate(dir, fixture.DefaultSpec(fixture.Valid))
	if err != nil {
		t.Fatal(err)
	}
	pf, err := probe.Analyze(gen.Path, probe.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	pf.Tracks[0].Timescale = 0 // force the video layer to fail
	rcfg := repair.DefaultConfig()
	rcfg.Recover = true
	res := repair.New(rcfg, probe.DefaultConfig()).Recover(pf)
	if res.Status != repair.ResultPartial {
		t.Fatalf("status=%s want partial", res.Status)
	}
	if res.Tracks[0].Status != repair.StatusUnrecoverable {
		t.Fatalf("track 0 status=%s", res.Tracks[0].Status)
	}
	if res.Tracks[1].Status == repair.StatusUnrecoverable {
		t.Fatal("audio was not preserved")
	}
}

func probeTrackByID(pf *probe.File, id uint32) *probe.Track {
	for _, t := range pf.Tracks {
		if t.ID == id {
			return t
		}
	}
	return nil
}
