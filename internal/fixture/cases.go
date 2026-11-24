package fixture

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"mediad/internal/metadata"
	"mediad/internal/mp4"
)

func fragmentPath(cfg Config, d Damage) string {
	camera := cfg.Camera
	if camera == "" {
		camera = "cam01"
	}
	return filepath.Join(cfg.Root, camera, d.Rep, segBase(d.Index)+".m4s")
}

func applyDamage(cfg Config, d Damage) error {
	path := fragmentPath(cfg, d)
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("damage target missing: %w", err)
	}
	switch d.Kind {
	case "truncate":
		if len(raw) <= 24 {
			return fmt.Errorf("fragment too short to truncate: %s", path)
		}
		return os.WriteFile(path, raw[:len(raw)-24], 0o644)
	case "oob_count":
		return corruptTrunCount(raw, path)
	default:
		return fmt.Errorf("unknown damage kind %q", d.Kind)
	}
}

func corruptTrunCount(raw []byte, path string) error {
	moof, err := mp4.ReadHeader(raw, 0)
	if err != nil {
		return err
	}
	traf, _, err := mp4.FindDirectChild(raw, moof, mp4.BoxTraf)
	if err != nil {
		return err
	}
	trun, _, err := mp4.FindDirectChild(raw, traf, mp4.BoxTrun)
	if err != nil {
		return err
	}
	countOff := trun.Offset + 8 + 4
	binary.BigEndian.PutUint32(raw[countOff:countOff+4], 0xFFFFFFFF)
	return os.WriteFile(path, raw, 0o644)
}

// CleanConfig is the pristine 3x8 ladder used by the clean demo.
func CleanConfig(root string, seed int64) Config {
	return Config{Root: root, Seed: seed, FragmentsPerRep: 8, Reps: DefaultReps()}
}

// DamagedConfig is the same ladder with exactly two structural corruptions.
func DamagedConfig(root string, seed int64) Config {
	cfg := CleanConfig(root, seed)
	cfg.Damage = []Damage{
		{Rep: "high", Index: 2, Kind: "oob_count"},
		{Rep: "mid", Index: 5, Kind: "truncate"},
	}
	return cfg
}

// CancelConfig is a small deterministic single-representation tree so the
// cancellation gate can leave exactly one uncommitted fragment.
func CancelConfig(root string) Config {
	return Config{Root: root, Seed: 23, FragmentsPerRep: 4, Reps: []Rep{HighRep()}}
}

// MetadataConflictConfig yields one representation whose raw color differs and
// whose audio rate differs, exercising both split policies.
func MetadataConflictConfig(root string, seed int64) Config {
	reps := DefaultReps()
	reps[2].Color = metadataColorBT2020HLG()
	reps[2].Audio.SampleRateHz = 44100
	reps[2].Audio.SampleRateRaw = 44100 << 16
	return Config{Root: root, Seed: seed, FragmentsPerRep: 8, Reps: reps}
}

func metadataColorBT2020HLG() metadata.Color {
	return metadata.Color{ColorType: "nclx", Primaries: 9, Transfer: 18, Matrix: 9}
}
