package runner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"os"
	"path/filepath"

	"fieldcut/internal/media"
	"fieldcut/internal/render"
)

// Verification is the independent check of an exported directory.
type Verification struct {
	Frames          int
	RootOnDisk      string
	RootExpected    string
	SyncMaxMs       float64
	RecoveryRate    float64
	Eligible        int
	Recovered       int
	P95FrameMs      float64
	Missing         int
	CorruptRepaired int
	AudioOffsetNs   int64
	Pulses          int
}

// Verify re-derives every expected frame from the generated input, hashes
// the bytes on disk and compares the root, frame count and recorded metrics.
// It never trusts export.json alone.
func Verify(outDir, inputDir string) (*Verification, error) {
	own, err := readOwnership(outDir)
	if err != nil {
		return nil, err
	}
	a, err := analyze(Options{OutDir: outDir, InputDir: inputDir})
	if err != nil {
		return nil, err
	}
	if own.Width != a.width || own.Height != a.height || own.Frames != a.frames {
		return nil, media.Newf(media.KindConflict, "verify.shape", "", -1,
			"export says %dx%d x %d frames, input says %dx%d x %d",
			own.Width, own.Height, own.Frames, a.width, a.height, a.frames)
	}
	h := sha256.New()
	for idx := int64(0); idx < int64(a.frames); idx++ {
		want, err := a.composeFrame(idx)
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, want); err != nil {
			return nil, media.New(media.KindCorrupt, "verify.encode", "", idx, err)
		}
		sum := sha256.Sum256(buf.Bytes())
		got, err := hashDiskFrame(filepath.Join(outDir, "frames", fmt.Sprintf("%05d.png", idx)))
		if err != nil {
			return nil, media.New(media.KindConflict, "verify.frame", "", idx, err)
		}
		if got != hex.EncodeToString(sum[:]) {
			return nil, media.Newf(media.KindConflict, "verify.frame", "", idx,
				"frame bytes differ from the deterministic recomputation")
		}
		fmt.Fprintf(h, "%d %s\n", idx, got)
	}
	root := hex.EncodeToString(h.Sum(nil))
	v := &Verification{
		Frames: a.frames, RootOnDisk: root, RootExpected: own.Root,
		SyncMaxMs: a.syncMaxMs, RecoveryRate: a.rate,
		Eligible: a.eligible, Recovered: a.recovered,
		Missing: a.missing, CorruptRepaired: a.corruptRepaired,
		AudioOffsetNs: a.audio.OffsetNanos, Pulses: len(a.audio.Pulses),
		// The export records the measured composition percentile; nothing
		// on disk can be re-timed, so verification uses the recorded value.
		P95FrameMs: own.P95FrameMs,
	}
	if root != own.Root {
		return nil, media.Newf(media.KindConflict, "verify.root", "", -1,
			"root %s does not match recorded %s", root, own.Root)
	}
	if own.AudioOffsetNs != a.audio.OffsetNanos || own.AudioPulses != len(a.audio.Pulses) {
		return nil, media.New(media.KindConflict, "verify.audio", "", -1,
			errors.New("recorded audio alignment differs from recomputation"))
	}
	if own.Eligible != a.eligible || own.Recovered != a.recovered {
		return nil, media.New(media.KindConflict, "verify.recovery", "", -1,
			errors.New("recorded recovery counts differ from recomputation"))
	}
	if own.SyncMaxMs != a.syncMaxMs || own.RecoveryRate != a.rate {
		return nil, media.Newf(media.KindConflict, "verify.metrics", "", -1,
			"recorded sync=%.3f rate=%.4f, recomputed sync=%.3f rate=%.4f",
			own.SyncMaxMs, own.RecoveryRate, a.syncMaxMs, a.rate)
	}
	return v, nil
}

func readOwnership(dir string) (*render.Ownership, error) {
	data, err := os.ReadFile(filepath.Join(dir, render.ManifestName))
	if err != nil {
		return nil, media.New(media.KindConflict, "verify.manifest", "", -1, err)
	}
	var own render.Ownership
	if err := json.Unmarshal(data, &own); err != nil || own.Program != "fieldcut" {
		return nil, media.New(media.KindConflict, "verify.manifest", "", -1,
			errors.New("output is not a fieldcut export"))
	}
	return &own, nil
}

func hashDiskFrame(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
