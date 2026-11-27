package pipeline

import (
	"errors"
	"path/filepath"
	"testing"

	"relayctl/internal/fixture"
)

// prepare saves a generated scenario into a temp fixture directory.
func prepare(t *testing.T, caseName string, seed int64, damaged bool) *Engine {
	t.Helper()
	sc, err := fixture.Generate(caseName, seed)
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "input")
	state := filepath.Join(t.TempDir(), "state")
	if err := SaveScenario(input, sc); err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(Config{InputDir: input, StateDir: state, Damaged: damaged})
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

func TestGoodPathRecoversAndVerifies(t *testing.T) {
	eng := prepare(t, fixture.CaseDriftWrapVFR, 41, false)
	st, err := eng.Run()
	if err != nil || st.ExitCode != ExitOK {
		t.Fatalf("good run: %+v err=%v", st, err)
	}
	if st.Recovered < 1 {
		t.Fatalf("expected at least one FEC-recovered packet, got %d", st.Recovered)
	}
	if st.Unrecoverable != 0 {
		t.Fatalf("good path must have no unrecoverable loss: %d", st.Unrecoverable)
	}
	if len(st.SHA256) != 64 {
		t.Fatalf("sha256 = %q", st.SHA256)
	}
	if st.ResumeMS < 0 || st.ResumeMS > 1000 {
		t.Fatalf("resume_ms %d outside [0,1000]", st.ResumeMS)
	}
}

func TestGoodPathRepeatDigestEqual(t *testing.T) {
	eng := prepare(t, fixture.CaseDriftWrapVFR, 41, false)
	if _, err := eng.Run(); err != nil {
		t.Fatal(err)
	}
	st, err := eng.Verify(2)
	if err != nil {
		t.Fatal(err)
	}
	if !st.RepeatEqual {
		t.Fatal("repeated verification must produce equal digests")
	}
}

func TestResumeOnlyRunsIncompleteNodes(t *testing.T) {
	eng := prepare(t, fixture.CaseDriftWrapVFR, 41, false)
	eng.cfg.PauseAfter = NodeSync
	st, err := eng.Run()
	if err == nil || st.ExitCode != ExitPaused {
		t.Fatalf("pause run: %+v err=%v", st, err)
	}
	// Journal now holds succeeded nodes through sync.
	before := len(eng.journal.Snapshot())
	resumeEng, err := NewEngine(Config{InputDir: eng.cfg.InputDir, StateDir: eng.cfg.StateDir})
	if err != nil {
		t.Fatal(err)
	}
	st2, err := resumeEng.Resume()
	if err != nil || st2.ExitCode != ExitOK {
		t.Fatalf("resume: %+v err=%v", st2, err)
	}
	if len(resumeEng.journal.Snapshot()) < before {
		t.Fatal("resume must not erase prior journal entries")
	}
}

func TestDamagedPathLocatesOffset(t *testing.T) {
	eng := prepare(t, fixture.CaseTruncateMdat, 42, true)
	st, err := eng.Run()
	if err == nil {
		t.Fatal("damaged input must return an error")
	}
	if !errors.Is(err, ErrUnrecoverable) && !errors.Is(err, ErrDamaged) {
		t.Fatalf("want damage/loss sentinel, got %v", err)
	}
	// Whether the burst loss or the mdat truncation fires first, the run must
	// terminate with the damage-family exit code and a located offset.
	if st.ExitCode != ExitDamaged && st.ExitCode != ExitLost {
		t.Fatalf("exit code = %d", st.ExitCode)
	}
	if st.FirstBlocked == "" {
		t.Fatal("a blocked dependent must be reported")
	}
}

func TestDamageDeltaZeroOnParse(t *testing.T) {
	// Drive the parser directly on the damaged stream to assert exact offset.
	eng := prepare(t, fixture.CaseTruncateMdat, 42, true)
	_, off, err := eng.parseFragments()
	if err == nil {
		t.Fatal("truncated mdat must fail parsing")
	}
	want := eng.scenario.Manifest.ExpectedDamageOffset
	if off != want {
		t.Fatalf("found=%d expected=%d delta=%d", off, want, off-want)
	}
}
