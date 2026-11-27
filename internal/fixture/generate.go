// Package fixture generates deterministic synthetic MP4/MOV material and
// fault profiles so every mediad path can be exercised without external
// media or network access.
package fixture

import (
	"encoding/json"
	"os"
	"path/filepath"

	"mediad/internal/index"
)

// Spec names the fixture classes the generator can emit.
type Spec struct {
	Dir string
}

// LossProfileName is the fault profile consumed by the deliver command.
const LossProfileName = "loss.json"

// Names of the generated source files.
const (
	NameNormal    = "normal.mp4"
	NameCorrupt   = "corrupt.mp4"
	NameTruncated = "truncated.mp4"
	NameWrap      = "wraparound.mp4"
	NameInterrupt = "interrupt.mp4"
)

func fillByte(i int) byte { return byte((i*37 + 11) & 0xFF) }

// videoSamples builds n H.264-like sample specs at 30 fps on a 90kHz clock.
func videoSamples(n int) []index.SampleSpec {
	s := make([]index.SampleSpec, n)
	for i := range s {
		s[i] = index.SampleSpec{Size: 4096, DTSStep: 3000, Key: i%10 == 0}
	}
	return s
}

// audioSamples builds n AAC-LC-like 1024-sample frames at 48 kHz.
func audioSamples(n int) []index.SampleSpec {
	s := make([]index.SampleSpec, n)
	for i := range s {
		s[i] = index.SampleSpec{Size: 256, DTSStep: 1024, Key: true}
	}
	return s
}

// Generate writes the full fixture set into dir. Total media size is sized
// to roughly 10 MiB so resume timing can be measured against the brief.
func Generate(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// ~10MiB: 2560 video frames * 4096B ≈ 10 MiB
	video := videoSamples(2560)
	audio := audioSamples(2560)

	normal := index.BuildMP4(true, 90000, video, false, fillByte)
	if err := writeFile(filepath.Join(dir, NameNormal), normal); err != nil {
		return err
	}

	// corrupt: flip one byte inside the mdat payload and corrupt an stco
	corrupt := index.BuildMP4(true, 90000, video, false, fillByte)
	corrupt[len(corrupt)-64] ^= 0xFF
	if err := writeFile(filepath.Join(dir, NameCorrupt), corrupt); err != nil {
		return err
	}

	// truncated: cut the file inside the trailing region
	trunc := index.BuildMP4(true, 90000, video, false, fillByte)
	cut := len(trunc) * 9 / 10
	if err := writeFile(filepath.Join(dir, NameTruncated), trunc[:cut]); err != nil {
		return err
	}

	// wraparound: a small timescale so cumulative units approach the
	// 32-bit boundary, then continue past it to force a modular wrap
	wrap := buildWraparound()
	if err := writeFile(filepath.Join(dir, NameWrap), wrap); err != nil {
		return err
	}

	// interrupt target is a clean, independently named copy of normal media
	if err := writeFile(filepath.Join(dir, NameInterrupt), normal); err != nil {
		return err
	}

	// audio companion used by the A/V sync path
	audioBytes := index.BuildMP4(false, 48000, audio, true, fillByte)
	if err := writeFile(filepath.Join(dir, "audio.m4a"), audioBytes); err != nil {
		return err
	}

	return writeLossProfile(dir)
}

// buildWraparound crafts a track whose raw PTS units cross 2^32. DTS climbs
// in uniform steps; a negative composition offset on the wrap sample pulls
// its raw PTS just past the 32-bit modulus boundary so the stored modular
// value jumps backward, which the scanner must classify as a wraparound.
func buildWraparound() []byte {
	const step = int64(1 << 30) // 4 samples span the full 32-bit modulus
	n := 12
	specs := make([]index.SampleSpec, n)
	for i := range specs {
		specs[i] = index.SampleSpec{Size: 64, DTSStep: uint32(step), Key: true}
	}
	// sample 4 DTS = 4*step = 2^32; apply a composition offset so its raw
	// PTS lands at mod32+100, whose 32-bit modular encoding (100) is far
	// below the previous sample's modular value (~3*step).
	specs[4].CTOff = int32(100 - step)
	return index.BuildMP4(true, 1, specs, false, fillByte)
}

// writeLossProfile emits a deterministic profile guaranteeing at least one
// recoverable single-packet loss and at least one unrecoverable burst.
func writeLossProfile(dir string) error {
	profile := map[string]any{
		"seed":           42,
		"reorder_rate":   0.15,
		"duplicate_rate": 0.05,
		"loss_rate":      0.0,
		"burst_size":     1,
		// drop sequence 4 (XOR-recoverable via survivor 5) and the pair
		// 8,9 (both members of one block -> permanent gap)
		"drop_seqs": []uint16{4, 8, 9},
	}
	b, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, LossProfileName), b, 0o644)
}

func writeFile(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
