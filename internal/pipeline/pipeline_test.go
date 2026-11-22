package pipeline

import "testing"

func baseCfg(dir string) Config {
	return Config{
		Seed: 41, Packets: 2000, Chunk: 64, LossBurst: 13,
		Bandwidth: []float64{0.2, 4.0, 0.5}, Dir: dir,
	}
}

// Clean path: lossless handling of duplicates/reorder, burst loss
// counted, and identical hashes across repeated runs.
func TestCleanRunDeterministic(t *testing.T) {
	r1, err := Run(baseCfg(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if r1.ExitCode != 0 {
		t.Fatalf("exit = %d", r1.ExitCode)
	}
	if r1.MissingPackets != 13 {
		t.Fatalf("missing = %d, want 13", r1.MissingPackets)
	}
	r2, err := Run(baseCfg(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if r1.FrameSHA256 != r2.FrameSHA256 || r1.OutputSHA256 != r2.OutputSHA256 ||
		r1.TrackSHA256 != r2.TrackSHA256 {
		t.Fatal("repeated run produced different hashes")
	}
}

// Corruption path: the injected subtitle byte must be located within
// one byte and the run must exit 2 with a defect record.
func TestCorruptSubtitleLocated(t *testing.T) {
	cfg := baseCfg(t.TempDir())
	cfg.Corrupt = "subtitle"
	r, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if r.ExitCode != 2 {
		t.Fatalf("exit = %d, want 2", r.ExitCode)
	}
	_, want := generateSRT(cfg.Packets/4, cfg.Seed, true)
	if d := r.BadOffsetBytes - want; d < -1 || d > 1 {
		t.Fatalf("bad_offset_bytes = %d, want %d ±1", r.BadOffsetBytes, want)
	}
	found := false
	for _, d := range r.Defects {
		if d.Category == "subtitle_parse" && d.Offset == r.BadOffsetBytes {
			found = true
		}
	}
	if !found {
		t.Fatal("no subtitle_parse defect at reported offset")
	}
}

// Interrupt path: resume must reproduce the uninterrupted hashes.
func TestInterruptResumeMatchesClean(t *testing.T) {
	clean, err := Run(baseCfg(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	cfg := baseCfg(t.TempDir())
	cfg.InterruptAfter = 5
	cfg.Resume = true
	r, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if r.ExitCode != 0 {
		t.Fatalf("exit = %d", r.ExitCode)
	}
	if r.OutputSHA256 != clean.OutputSHA256 || r.FrameSHA256 != clean.FrameSHA256 ||
		r.TrackSHA256 != clean.TrackSHA256 {
		t.Fatal("resumed run diverged from clean run")
	}
	if r.MissingPackets != clean.MissingPackets {
		t.Fatalf("missing = %d, want %d", r.MissingPackets, clean.MissingPackets)
	}
	if r.ResumeMs >= 5000 {
		t.Fatalf("resume took %d ms", r.ResumeMs)
	}
}

// Corrupt index: recovery must fall back to the last valid checkpoint
// and still produce the stable checksums.
func TestCorruptIndexRecovers(t *testing.T) {
	clean, err := Run(baseCfg(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	cfg := baseCfg(t.TempDir())
	cfg.InterruptAfter = 5
	cfg.Resume = true
	cfg.Corrupt = "index"
	r, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if r.ExitCode != 0 {
		t.Fatalf("exit = %d", r.ExitCode)
	}
	// Chunks 0..4 were committed, chunk 4 was corrupted -> recover to 3.
	if r.LastValidCheckpoint != 3 {
		t.Fatalf("last valid = %d, want 3", r.LastValidCheckpoint)
	}
	if r.OutputSHA256 != clean.OutputSHA256 {
		t.Fatal("checksum not stable after index recovery")
	}
}

// Corrupt output tail: recovery must truncate to a verifiable cursor.
func TestCorruptOutputRecovers(t *testing.T) {
	clean, err := Run(baseCfg(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	cfg := baseCfg(t.TempDir())
	cfg.InterruptAfter = 5
	cfg.Resume = true
	cfg.Corrupt = "output"
	r, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if r.ExitCode != 0 || r.OutputSHA256 != clean.OutputSHA256 {
		t.Fatalf("exit = %d, checksum unstable", r.ExitCode)
	}
}

// Acceptance scale: 12000 packets, 256-packet chunks, interrupt+resume.
func TestAcceptanceScale(t *testing.T) {
	cfg := Config{
		Seed: 41, Packets: 12000, Chunk: 256, LossBurst: 37,
		Bandwidth: []float64{0.2, 4.0, 0.5},
		InterruptAfter: 20, Resume: true, Dir: t.TempDir(),
	}
	r, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if r.ExitCode != 0 {
		t.Fatalf("exit = %d", r.ExitCode)
	}
	if r.MissingPackets != 37 {
		t.Fatalf("missing = %d, want 37", r.MissingPackets)
	}
	if r.ResumeMs >= 5000 {
		t.Fatalf("resume took %d ms, want < 5000", r.ResumeMs)
	}
	if r.FrameCount == 0 || r.FrameSHA256 == "" || r.OutputSHA256 == "" {
		t.Fatal("missing frame/output data")
	}
}
