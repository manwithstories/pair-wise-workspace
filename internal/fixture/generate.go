// Package fixture synthesizes deterministic MP4/MOV inputs for tests and the
// demo command. Every byte is produced locally; nothing external is read.
package fixture

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

// Kind names a generated scenario.
type Kind string

const (
	Valid            Kind = "valid"
	Truncated        Kind = "truncated"
	Corrupt          Kind = "corrupt"
	MissingIndex     Kind = "missing-index"
	BoundaryMismatch Kind = "boundary-mismatch"
	Empty            Kind = "empty"
)

// Spec controls one generated file.
type Spec struct {
	Kind      Kind
	Frames    int // video sample count
	AudioRate int // audio timescale
	SegmentMS int64
	// KeyframeEvery is the GOP length in frames.
	KeyframeEvery int
	// CorruptAt picks the sample index that receives damage.
	CorruptAt int
	// TruncateBytes removes this many tail bytes.
	TruncateBytes int64
}

// DefaultSpec returns a file with 3 s of video at 30 fps and 2 s of audio.
func DefaultSpec(kind Kind) Spec {
	return Spec{
		Kind:          kind,
		Frames:        90,
		AudioRate:     48000,
		SegmentMS:     2000,
		KeyframeEvery: 30,
		CorruptAt:     45,
		TruncateBytes: 4096,
	}
}

// Result reports what was written.
type Result struct {
	Path    string
	Kind    Kind
	Bytes   int64
	SHA256  string
	Samples int
	// Damage bounds for recovery-oriented kinds, else -1.
	DamagedSample int
	DamageOffset  int64
}

// Generate writes one fixture and returns its metadata.
func Generate(dir string, spec Spec) (Result, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, fmt.Errorf("create fixture dir: %w", err)
	}
	spec = spec.withDefaults()
	built, err := build(spec)
	if err != nil {
		return Result{}, err
	}
	data := built.bytes
	name := string(spec.Kind) + ".mp4"

	switch spec.Kind {
	case Truncated:
		if spec.TruncateBytes < int64(len(data)) {
			data = data[:int64(len(data))-spec.TruncateBytes]
		}
	case Corrupt:
		data = built.corrupt(spec)
	case Empty:
		data = nil
	}

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return Result{}, fmt.Errorf("write fixture: %w", err)
	}
	sum := sha256.Sum256(data)
	return Result{
		Path:          path,
		Kind:          spec.Kind,
		Bytes:         int64(len(data)),
		SHA256:        fmt.Sprintf("%x", sum),
		Samples:       spec.Frames,
		DamagedSample: built.damaged,
		DamageOffset:  built.damageOffset,
	}, nil
}

// GenerateAll writes the standard fixture set and returns results in a stable
// order. Any failure aborts so tests and demos see a complete set or none.
func GenerateAll(dir string) ([]Result, error) {
	kinds := []Kind{Valid, Truncated, Corrupt, MissingIndex, BoundaryMismatch}
	out := make([]Result, 0, len(kinds))
	for _, k := range kinds {
		r, err := Generate(dir, DefaultSpec(k))
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (s Spec) withDefaults() Spec {
	if s.Frames <= 0 {
		s.Frames = 90
	}
	if s.AudioRate <= 0 {
		s.AudioRate = 48000
	}
	if s.SegmentMS <= 0 {
		s.SegmentMS = 2000
	}
	if s.KeyframeEvery <= 0 {
		s.KeyframeEvery = 30
	}
	if s.CorruptAt <= 0 || s.CorruptAt >= s.Frames {
		s.CorruptAt = s.Frames / 2
	}
	if s.TruncateBytes <= 0 {
		s.TruncateBytes = 4096
	}
	return s
}

// built holds the encoded file plus anchors used to apply damage.
type built struct {
	bytes          []byte
	movieTimescale uint32
	videoSamples   int
	sampleOffsets  []int64
	sampleSizes    []int
	audioTrackOff  int64
	damaged        int
	damageOffset   int64
}

// corrupt overwrites the payload of one sample with a marker byte. The
// surrounding box structure stays valid, so the damage is local.
func (b *built) corrupt(spec Spec) []byte {
	if b.damaged < 0 || b.damaged >= len(b.sampleOffsets) {
		return append([]byte(nil), b.bytes...)
	}
	out := append([]byte(nil), b.bytes...)
	start := b.sampleOffsets[b.damaged]
	end := start + int64(b.sampleSizes[b.damaged])
	if end > int64(len(out)) {
		end = int64(len(out))
	}
	for i := start; i < end; i++ {
		out[i] = 0xFF
	}
	return out
}
