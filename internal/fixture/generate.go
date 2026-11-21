package fixture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Bundle is the on-disk set of generated fixtures.
type Bundle struct {
	Dir       string
	StudioMP4 string
	CameraMXF string
	RTPStream string
	Plans     []PlanRecord
	Duration  int64 // unified duration in ns
}

// PlanRecord captures the receive order so tests can assert replays.
type PlanRecord struct {
	Name      string
	Order     []int
	Duplicate int
	Missing   int
}

// Generate writes all fixtures under outDir, which must be inside the caller's
// output root. It never touches any path outside that directory.
func Generate(outDir string, opts Options) (Bundle, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return Bundle{}, err
	}
	rates := effectiverates(opts.VFR)
	studio := studioTracks(opts.Seed)
	camera := cameraTracks(opts.Seed, rates)

	studioBytes := buildMP4(studio)
	cameraBytes := buildMXF(camera, false)
	cameraCorrupt := buildMXF(camera, true)

	plan := PlanDelivery(len(camera[0].Samples), 12)
	rtpBytes := buildRTPPackets(camera[0].Samples, opts.DriftPPM, opts.WrapAt, plan)

	b := Bundle{
		Dir:       outDir,
		StudioMP4: filepath.Join(outDir, "studio.mp4"),
		CameraMXF: filepath.Join(outDir, "camera.mxf"),
		RTPStream: filepath.Join(outDir, "camera.rtp"),
		Plans:     []PlanRecord{{Name: "camera", Order: plan.Order, Duplicate: plan.Duplicate, Missing: plan.Missing}},
	}
	files := []struct {
		path string
		data []byte
	}{
		{b.StudioMP4, studioBytes},
		{b.CameraMXF, cameraBytes},
		{b.RTPStream, rtpBytes},
		{filepath.Join(outDir, "camera_corrupt.mxf"), cameraCorrupt},
	}
	for _, f := range files {
		if len(f.data) > 8<<20 {
			return Bundle{}, fmt.Errorf("fixture %s exceeds 8 MiB", f.path)
		}
		if err := os.WriteFile(f.path, f.data, 0o644); err != nil {
			return Bundle{}, err
		}
	}
	// Sidecar manifest records the exact sizes and hashes of what was written.
	if err := writeSidecar(b, files); err != nil {
		return Bundle{}, err
	}
	return b, nil
}

func effectiverates(v []int64) []int64 { return defaultVFR(v) }

type sidecar struct {
	Files []sidecarFile `json:"files"`
	Plans []PlanRecord  `json:"plans"`
}

type sidecarFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Hash string `json:"sha256"`
}

func writeSidecar(b Bundle, files []struct {
	path string
	data []byte
}) error {
	var sc sidecar
	for _, f := range files {
		sum := sha256.Sum256(f.data)
		sc.Files = append(sc.Files, sidecarFile{
			Name: filepath.Base(f.path), Size: int64(len(f.data)), Hash: hex.EncodeToString(sum[:]),
		})
	}
	sc.Plans = b.Plans
	enc, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(b.Dir, "fixtures.json"), enc, 0o644)
}
