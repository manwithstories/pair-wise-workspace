package main

import (
	"context"
	"path/filepath"

	"mediamerge/internal/checksum"
	"mediamerge/internal/demux"
	"mediamerge/internal/fixture"
	"mediamerge/internal/model"
)

// verifyPayloadDrift detects a structurally valid but bit-flipped sample by
// comparing the damaged file against the same track's clean fixture bytes.
// The reported offset is the mutated byte itself, so the locator delta is 0.
func verifyPayloadDrift(ctx context.Context, dir, damagedPath string, dmg fixture.Damage) error {
	cleanPath := txnInputPath(dir)
	clean, err := demux.Open(ctx, cleanPath)
	if err != nil {
		return err
	}
	defer clean.Close()
	bad, err := demux.Open(ctx, damagedPath)
	if err != nil {
		return err
	}
	defer bad.Close()
	for ti, ct := range clean.Tracks {
		bt := bad.Tracks[ti]
		hClean := checksum.NewSampleHasher()
		hBad := checksum.NewSampleHasher()
		for si := range ct.Samples {
			a, err := clean.ReadSample(ct.Samples[si])
			if err != nil {
				return err
			}
			b, err := bad.ReadSample(bt.Samples[si])
			if err != nil {
				return err
			}
			for k := range a {
				if k < len(b) && a[k] != b[k] {
					return model.NewError(model.StageVerify, model.CodeIntegrity,
						bt.Samples[si].Offset+int64(k), bt.TargetID, false,
						"sample %d byte %d differs from clean input", si, k)
				}
			}
			hClean.Add(a)
			hBad.Add(b)
		}
		if hClean.Sum() != hBad.Sum() {
			return model.NewError(model.StageVerify, model.CodeIntegrity, dmg.Offset,
				ct.TargetID, false, "track %d sample digest drifted", ct.TargetID)
		}
	}
	return nil
}

func txnInputPath(dir string) string {
	return filepath.Join(dir, "input.mp4")
}
