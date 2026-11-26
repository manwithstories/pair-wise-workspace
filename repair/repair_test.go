package repair_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"archivectl/internal/core"
	"archivectl/internal/fixture"
	"archivectl/internal/index"
	irepair "archivectl/internal/repair"
	"archivectl/internal/verify"
	iwav "archivectl/internal/wavscan"
)

func TestRepairPropagatesDamageAndMatchesBaseline(t *testing.T) {
	dir := t.TempDir()
	m, idx := buildDamagedIndex(t, dir, 17)
	out := filepath.Join(dir, "repaired.wav")
	res, err := irepair.Run(irepair.Options{Index: idx, OutputPath: out})
	if err != nil {
		t.Fatal(err)
	}
	if res.RecoveredFrames <= 0 {
		t.Fatal("expected recovered frames")
	}
	cov, err := irepair.NewCoverage(idx.Damage)
	if err != nil {
		t.Fatal(err)
	}
	if res.RecoveredFrames != cov.Frames() {
		t.Fatalf("recovered=%d coverage=%d", res.RecoveredFrames, cov.Frames())
	}
	for _, d := range idx.Damage {
		covered := false
		for _, c := range res.Coverage {
			if c.StartFrame <= d.StartFrame && d.EndFrame <= c.EndFrame {
				covered = true
				break
			}
		}
		if !covered {
			t.Fatalf("damage span %+v not propagated to repair coverage %+v", d, res.Coverage)
		}
	}

	outRep, err := iwav.ScanPath(out)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := verify.HashWAVPath(out, outRep.DataOffset, outRep.Format.FrameBytes(), outRep.DeclaredFrames)
	if err != nil {
		t.Fatal(err)
	}
	baseRep, err := iwav.ScanPath(m.BaselineWAV)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := verify.HashWAVPath(m.BaselineWAV, baseRep.DataOffset, baseRep.Format.FrameBytes(), baseRep.DeclaredFrames)
	if err != nil {
		t.Fatal(err)
	}
	cmp := verify.CompareSegments(actual, expected)
	if err := cmp.AllMatch(); err != nil {
		t.Fatal(err)
	}
	if cmp.Total != 8 {
		t.Fatalf("segments=%d want 8 for 32768 frames", cmp.Total)
	}

	out2 := filepath.Join(dir, "repaired-2.wav")
	if _, err := irepair.Run(irepair.Options{Index: idx, OutputPath: out2}); err != nil {
		t.Fatal(err)
	}
	h1, err := verify.FileSHA256(out)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := verify.FileSHA256(out2)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatal("rerun bytes/hashes differ")
	}
}

func TestRepairRejectsGapOverFrameCacheLimit(t *testing.T) {
	_, err := irepair.NewCoverage([]core.Span{{
		Kind: "missing_block", StartFrame: 0,
		EndFrame: int64(core.MaxCachedFrames) + 1, Recoverable: true,
	}})
	if !core.AsKind(err, core.KindDamageUnrecoverable) {
		t.Fatalf("expected unrecoverable limit error, got %v", err)
	}
}

func TestRepairInputRaceAbortsWithoutCommittedOutput(t *testing.T) {
	dir := t.TempDir()
	_, idx := buildDamagedIndex(t, dir, 19)
	out := filepath.Join(dir, "race-output.wav")
	_, err := irepair.Run(irepair.Options{
		Index: idx, OutputPath: out,
		AfterWriteBeforeRevalidate: func() error {
			f, err := os.OpenFile(idx.WAVPath, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = f.Write([]byte("race"))
			return err
		},
	})
	if !core.AsKind(err, core.KindInputRace) {
		t.Fatalf("expected input race error, got %v", err)
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("output must not be committed after race; stat err=%v", statErr)
	}
}

func TestCheckBlocksClassifiesMissingAndTruncatedRegions(t *testing.T) {
	data := make([]byte, 20) // 10 mono 16-bit frames
	goodCRC, err := verify.CRC32PCMRegion(bytes.NewReader(data), 0, 2, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	blocks := []verify.BlockCheck{
		{Index: 0, StartFrame: 0, FrameCount: 4, CRC32: goodCRC},
		{Index: 1, StartFrame: 100, FrameCount: 8, CRC32: 1},
		{Index: 2, StartFrame: 8, FrameCount: 4, CRC32: 2},
	}
	_, spans, err := verify.CheckBlocks(bytes.NewReader(data), 0, 2, 10, blocks)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, s := range spans {
		kinds[s.Kind] = true
	}
	if !kinds["missing_block"] || !kinds["truncation"] {
		t.Fatalf("expected missing_block and truncation spans, got %+v", spans)
	}
}

func buildDamagedIndex(t *testing.T, dir string, seed int64) (*fixture.Manifest, *index.Index) {
	t.Helper()
	m, err := fixture.Generate(fixture.Config{
		Dir: dir, Seed: seed, Frames: 32768, Channels: 2, Bits: 16,
		Injections:     []string{"vfr", "bad-crc", "truncate"},
		TruncateFrames: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := iwav.ScanPath(m.DamagedWAV)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := fixture.LoadClockFile(m.ClockJSON)
	if err != nil {
		t.Fatal(err)
	}
	checks, err := fixture.LoadChecksFile(m.ChecksJSON)
	if err != nil {
		t.Fatal(err)
	}
	_, spans, err := verify.CheckBlocksPath(m.DamagedWAV, rep.DataOffset,
		rep.Format.FrameBytes(), rep.AvailableFrames, checks.Blocks)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := index.Build(index.BuildInput{
		WAVPath: m.DamagedWAV, ClockPath: m.ClockJSON, ChecksPath: m.ChecksJSON,
		Report: rep, BlockRecords: cf.Blocks, BlockChecks: checks.Blocks, Damage: spans,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, idx
}
