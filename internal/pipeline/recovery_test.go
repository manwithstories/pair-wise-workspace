package pipeline_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/fault"
	"mediad/internal/pcm"
	"mediad/internal/pipeline"
	"mediad/internal/sample"
	"mediad/internal/txn"
)

func TestFaultsPropagateAndInterruptResume(t *testing.T) {
	dir := t.TempDir()
	m, err := sample.Generate(sample.Options{
		Seed: 29, Seconds: 1, Rate: 44100, Channels: 6, Dir: dir, Faults: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	badIndex := filepath.Join(dir, "index-working-bad.json")
	b, err := os.ReadFile(m.BadIndexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badIndex, b, 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "final.mp4")
	work := filepath.Join(dir, "work")
	cfg := pipeline.Config{
		InputPath: m.CapturePath, IndexPath: badIndex, OutputPath: out, WorkDir: work,
		Rate: m.Rate, Channels: m.Channels, PacketFrames: m.PacketFrames,
		ExpectedDamageOffset: m.BadCRCOffset,
	}

	inject := cfg
	inject.InjectStage = "mp4"
	if _, err := pipeline.Run(inject); fault.ExitCode(err) != fault.ExitInterrupted {
		t.Fatalf("inject err=%v code=%d", err, fault.ExitCode(err))
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output after interrupt exists: %v", err)
	}

	res, err := pipeline.Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rollbacks != 1 {
		t.Fatalf("rollbacks=%d", res.Rollbacks)
	}
	if res.Samples != 48000 {
		t.Fatalf("samples=%d", res.Samples)
	}
	if res.Reorder.Lost != m.LostInjections {
		t.Fatalf("lost=%d want %d", res.Reorder.Lost, m.LostInjections)
	}
	if res.Reorder.Reordered != m.ReorderInjections {
		t.Fatalf("reordered=%d want %d", res.Reorder.Reordered, m.ReorderInjections)
	}
	if res.Reorder.Duplicate != m.DuplicateInjections {
		t.Fatalf("duplicates=%d want %d", res.Reorder.Duplicate, m.DuplicateInjections)
	}
	if res.DamageOffsetError != 0 {
		t.Fatalf("damage offset error=%d", res.DamageOffsetError)
	}
	if !res.IndexRebuilt {
		t.Fatal("bad index was not reported as rebuilt")
	}
	if len(res.SHA256) != 64 {
		t.Fatalf("bad sha length %d", len(res.SHA256))
	}
	sum, _, err := txn.SHA256File(out)
	if err != nil || sum != res.SHA256 {
		t.Fatalf("file sha=%s result=%s err=%v", sum, res.SHA256, err)
	}

	again, err := pipeline.Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if again.SHA256 != res.SHA256 {
		t.Fatal("rerun changed MP4 checksum")
	}
}

func TestLostPacketsBecomeSilenceBeforeDSP(t *testing.T) {
	dir := t.TempDir()
	m, err := sample.Generate(sample.Options{
		Seed: 31, Seconds: 8, Rate: 48000, Channels: 2, Dir: dir, Faults: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := pipeline.Config{
		InputPath: m.CapturePath, IndexPath: m.IndexPath,
		OutputPath: filepath.Join(dir, "final.mp4"), WorkDir: filepath.Join(dir, "work"),
		Rate: m.Rate, Channels: m.Channels, PacketFrames: m.PacketFrames,
	}
	if _, err := pipeline.Run(cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "work", "decoded.l16"))
	if err != nil {
		t.Fatal(err)
	}
	samples, _, err := pcm.DecodeL16(raw, m.Channels)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != m.PacketFrames*m.RTPPackets*m.Channels {
		t.Fatalf("decoded samples=%d", len(samples))
	}

	for _, idx := range m.LostIndices {
		start := idx * m.PacketFrames * m.Channels
		end := (idx + 1) * m.PacketFrames * m.Channels
		for _, v := range samples[start:end] {
			if v != 0 {
				t.Fatalf("lost packet %d was not concealed with exact silence", idx)
			}
		}
	}
}
