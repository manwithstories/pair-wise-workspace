// Package fixture generates deterministic success, corrupt and edge-case
// media bundles with manifests, plus the expectations a run must match.
package fixture

import (
	"encoding/json"
	"os"
	"path/filepath"

	"mediad/internal/manifest"
	"mediad/internal/timeline"
)

// FPS is the fixture frame rate; StepPTS is the PTS increment at the
// shared 1/90000 time base.
const (
	FPS     = 24
	StepPTS = 3750
)

// Expectation lists the run outcomes a generated case must produce.
type Expectation struct {
	Frames          int              `json:"frames"`
	CorruptRanges   int              `json:"corrupt_ranges"`
	RecoveredFrames int              `json:"recovered_frames"`
	AudioSamples    map[string]int64 `json:"audio_samples,omitempty"`
}

// Generated locates one generated case directory and its expectation.
type Generated struct {
	Name   string
	Dir    string
	Expect Expectation
}

// GenerateAll builds the success, corrupt and edge cases under root.
func GenerateAll(root string, seed int64, frames int) ([]Generated, error) {
	var out []Generated
	for _, name := range []string{"success", "corrupt", "edge"} {
		g, err := generate(root, name, seed, frames)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

func generate(root, name string, seed int64, frames int) (Generated, error) {
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, "v1"), 0o755); err != nil {
		return Generated{}, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "a1"), 0o755); err != nil {
		return Generated{}, err
	}
	cfg := caseConfig(name, frames)
	m := &manifest.Manifest{
		Version:     1,
		Timebase:    timeline.Rational{Num: 1, Den: 90000},
		ColorPolicy: "srgb",
	}
	vt := manifest.Track{
		ID: "v1", Kind: "video", Codec: cfg.codec,
		FPS: &timeline.Rational{Num: FPS, Den: 1}, ColorTag: cfg.colorTag,
	}
	for seq := int64(0); seq < int64(frames); seq++ {
		pts := seq * StepPTS
		if seq == cfg.regressSeq {
			pts = (seq-1)*StepPTS - 100 // timestamp regression
		}
		data, err := encodeFrame(cfg.codec, frameImage(seed, seq))
		if err != nil {
			return Generated{}, err
		}
		written, sha := data, shaOf(data)
		switch seq {
		case cfg.hashBadSeq:
			sha = shaOf(append(data, 0x00)) // hash of bytes not on disk
		case cfg.truncSeq:
			written = data[:len(data)/2]
			sha = shaOf(written)
		}
		rel := filepath.Join("v1", frameName(cfg.codec, seq))
		if err := os.WriteFile(filepath.Join(dir, rel), written, 0o644); err != nil {
			return Generated{}, err
		}
		vt.Frames = append(vt.Frames, manifest.Frame{
			Seq: seq, PTS: pts, Keyframe: keyframeAt(name, seq),
			File: rel, SHA256: sha, Bytes: int64(len(written)),
		})
	}
	m.Tracks = append(m.Tracks, vt)

	at, audioFrames, err := audioTrack(dir, name, frames)
	if err != nil {
		return Generated{}, err
	}
	m.Tracks = append(m.Tracks, at)
	if err := writeManifest(dir, m); err != nil {
		return Generated{}, err
	}
	return Generated{Name: name, Dir: dir,
		Expect: expectation(cfg, frames, audioFrames)}, nil
}

type config struct {
	codec      string
	colorTag   string
	hashBadSeq int64 // -1: none
	truncSeq   int64
	regressSeq int64
}

func caseConfig(name string, frames int) config {
	n := int64(frames)
	switch name {
	case "corrupt":
		return config{codec: "png", colorTag: "bt709",
			hashBadSeq: n / 4, truncSeq: n / 2, regressSeq: -1}
	case "edge":
		return config{codec: "jpeg", colorTag: "acescg",
			hashBadSeq: -1, truncSeq: -1, regressSeq: n * 3 / 4}
	default:
		return config{codec: "png", colorTag: "srgb",
			hashBadSeq: -1, truncSeq: -1, regressSeq: -1}
	}
}

func keyframeAt(name string, seq int64) bool {
	if name == "edge" {
		return seq == 0 || seq == 55 // wide gap trips the interval check
	}
	return seq%12 == 0
}

func expectation(cfg config, frames int, audioFrames int64) Expectation {
	n := int64(frames)
	corrupt := []int64{}
	for _, s := range []int64{cfg.hashBadSeq, cfg.truncSeq, cfg.regressSeq} {
		if s >= 0 {
			corrupt = append(corrupt, s)
		}
	}
	bad := map[int64]bool{}
	for _, s := range corrupt {
		bad[s] = true
	}
	valid := n - int64(len(corrupt))
	recovered := 0
	if len(corrupt) > 0 {
		first := corrupt[0]
		for _, s := range corrupt {
			if s < first {
				first = s
			}
		}
		for seq := first + 1; seq < n; seq++ {
			if !bad[seq] {
				recovered++
			}
		}
	}
	e := Expectation{
		Frames:          int(valid),
		CorruptRanges:   len(corrupt),
		RecoveredFrames: recovered,
	}
	if audioFrames > 0 {
		e.AudioSamples = map[string]int64{"a1": audioFrames}
	}
	return e
}

func writeManifest(dir string, m *manifest.Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"),
		append(data, '\n'), 0o644)
}
