package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"mediaaudit/internal/clock"
	"mediaaudit/internal/fixture"
	"mediaaudit/internal/index"
	"mediaaudit/internal/model"
	"mediaaudit/internal/mp4"
)

// frameRangeType is an alias so the engine can reference model ranges without
// importing model in every signature.
type frameRangeType = model.FrameRange

func frameOf(v int64) model.Frame { return model.Frame(v) }

// loadConfig reads the manifest and builds the index configuration, including
// the input fingerprint used to reject resume after a package change.
func loadConfig(pkgDir string, chunkFrames, checkpointEvery int64) (index.Config, int64, error) {
	manPath := filepath.Join(pkgDir, fixture.ManifestFile)
	raw, err := os.ReadFile(manPath)
	if err != nil {
		return index.Config{}, 0, err
	}
	var man fixture.Manifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return index.Config{}, 0, err
	}
	// Fingerprint the actual audio bytes too, so content edits are detected.
	audioSum, err := hashFile(filepath.Join(pkgDir, fixture.AudioFile))
	if err != nil {
		return index.Config{}, 0, err
	}
	cfg := index.Config{
		PackagePath:      pkgDir,
		ChunkFrames:      chunkFrames,
		CheckpointEvery:  checkpointEvery,
		TileFrames:       man.TileFrames,
		InputFingerprint: man.Fingerprint + ":" + audioSum,
	}
	return cfg, man.Frames, nil
}

func hashFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// openOrCreateIndex loads an existing index or creates a fresh one. The bool
// reports whether the index was newly created.
func openOrCreateIndex(path string, cfg index.Config, totalFrames int64) (*index.File, bool, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		f, err := index.Create(path, cfg, totalFrames)
		return f, true, err
	}
	f, err := index.Load(path)
	if err != nil {
		return nil, false, err
	}
	return f, false, nil
}

// videoRateOf derives a unit-rate time base for cross-track mapping. The proxy
// timescale is ticks/second; frames are mapped via the timeline clock instead.
func videoRateOf(res *mp4.ScanResult) model.Rational {
	if len(res.Tracks) == 0 {
		return model.Hz(1)
	}
	return model.Hz(int64(res.Tracks[0].Timescale))
}

func appendFaultOnce(faults []model.Fault, f model.Fault) []model.Fault {
	for _, ex := range faults {
		if ex == f {
			return faults
		}
	}
	return append(faults, f)
}

func projectedFault(dm clock.DamageMap) model.Fault {
	t := dm.Targets[len(dm.Targets)-1]
	return model.Fault{
		Kind:       t.Kind,
		Track:      t.Track,
		Range:      model.FrameRange{First: t.Frame, Last: t.Frame + 1},
		ByteOffset: t.ByteOffset,
		Message:    "cross-track projected damage",
	}
}

// readReportSHA extracts the report_sha256 field from a stored report without
// depending on the full struct, so a tampered report is detected.
func readReportSHA(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", model.NewFault(model.KindIndex, "", model.FrameRange{}, -1,
			"report unmarshal: "+err.Error())
	}
	v, ok := doc["report_sha256"].(string)
	if !ok || v == "" {
		return "", model.NewFault(model.KindIndex, "", model.FrameRange{}, -1,
			"report missing report_sha256")
	}
	return v, nil
}
