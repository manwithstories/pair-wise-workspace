package pipeline

import (
	"path/filepath"
	"testing"
)

func baseCfg(t *testing.T, tamper bool) (Config, string) {
	return Config{
		WorkDir:     t.TempDir(),
		Seed:        41,
		Tracks:      3,
		Resume:      false,
		TamperCache: tamper,
	}, ""
}

func TestRunDeterministicAndMetrics(t *testing.T) {
	cfg, _ := baseCfg(t, false)
	r, err := NewRunner(cfg.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	m1, met1, err := r.Run(cfg)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if met1.RoundtripTotal == 0 || met1.RoundtripOK != met1.RoundtripTotal {
		t.Fatalf("roundtrip must be 100%%: %s", met1.Line())
	}
	if met1.HashTotal == 0 || met1.HashOK != met1.HashTotal {
		t.Fatalf("hash verification must be 100%%: %s", met1.Line())
	}
	if met1.KeyframeTotal == 0 {
		t.Fatal("keyframe checks must not be zero")
	}
	if rate := float64(met1.KeyframeHits) / float64(met1.KeyframeTotal); rate < 0.9 {
		t.Fatalf("keyframe hit rate %.2f below 0.9: %s", rate, met1.Line())
	}
	if met1.Incomplete == 0 {
		t.Fatal("fixture must produce incomplete segments")
	}

	// 干净重跑：工作目录独立，结果哈希应与首次一致。
	cfg2 := Config{WorkDir: t.TempDir(), Seed: 41, Tracks: 3}
	r2, _ := NewRunner(cfg2.WorkDir)
	m2, met2, err := r2.Run(cfg2)
	if err != nil {
		t.Fatal(err)
	}
	if len(m2.Segments) != len(m1.Segments) {
		t.Fatalf("segment count drift: %d vs %d", len(m2.Segments), len(m1.Segments))
	}
	for i := range m1.Segments {
		if m1.Segments[i].ContentDigest != m2.Segments[i].ContentDigest {
			t.Fatalf("digest drift on %s", m1.Segments[i].ID)
		}
	}
	if met2.Line() != met1.Line() {
		t.Fatalf("non-deterministic metrics:\n%s\n%s", met1.Line(), met2.Line())
	}
}

func TestResumeRecoversAndDoesNotDoubleCount(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{WorkDir: dir, Seed: 41, Tracks: 3, Resume: true}
	r, err := NewRunner(filepath.Join(dir, "runner"))
	if err != nil {
		t.Fatal(err)
	}
	// Runner 自带 cache/state 子目录，直接用同一工作区重跑。
	_, met1, err := r.Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, met2, err := r.Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if met2.Recovered == 0 {
		t.Fatalf("second run must recover segments: %s", met2.Line())
	}
	if met2.HashTotal != met1.HashTotal {
		t.Fatalf("hash count changed across resume: %d vs %d",
			met2.HashTotal, met1.HashTotal)
	}
	if met2.RoundtripOK != met1.RoundtripOK {
		t.Fatal("roundtrip count must stay stable on resume")
	}
}

func TestCacheTamperingRejected(t *testing.T) {
	cfg, _ := baseCfg(t, true)
	r, err := NewRunner(cfg.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	_, met, err := r.Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if met.CacheRejected == 0 {
		t.Fatalf("tampered cache must be rejected: %s", met.Line())
	}
}

func TestDifferentSeedProducesDifferentInput(t *testing.T) {
	cfg1 := Config{WorkDir: t.TempDir(), Seed: 41, Tracks: 2}
	cfg2 := Config{WorkDir: t.TempDir(), Seed: 99, Tracks: 2}
	r1, _ := NewRunner(cfg1.WorkDir)
	r2, _ := NewRunner(cfg2.WorkDir)
	m1, _, err := r1.Run(cfg1)
	if err != nil {
		t.Fatal(err)
	}
	m2, _, err := r2.Run(cfg2)
	if err != nil {
		t.Fatal(err)
	}
	if m1.InputID == m2.InputID {
		t.Fatal("different seeds must yield different input digests")
	}
}
