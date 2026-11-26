// Package manifest decodes and validates frame/audio track manifests.
// Decoding is strict: unknown fields, duplicate sequence numbers and
// missing required fields are rejected.
package manifest

import (
	"encoding/json"
	"fmt"
	"io"

	"mediad/internal/timeline"
)

// Manifest describes a multi-track media bundle.
type Manifest struct {
	Version     int               `json:"version"`
	Timebase    timeline.Rational `json:"timebase"`
	ColorPolicy string            `json:"color_policy"`
	Tracks      []Track           `json:"tracks"`
}

// Track is either a video frame sequence or a PCM audio track.
type Track struct {
	ID         string             `json:"id"`
	Kind       string             `json:"kind"` // "video" | "audio"
	Codec      string             `json:"codec,omitempty"`
	FPS        *timeline.Rational `json:"fps,omitempty"`
	ColorTag   string             `json:"color_tag,omitempty"`
	Layout     string             `json:"layout,omitempty"`
	SampleRate int                `json:"sample_rate,omitempty"`
	Format     string             `json:"format,omitempty"`
	PTS        int64              `json:"pts,omitempty"`
	File       string             `json:"file,omitempty"`
	SHA256     string             `json:"sha256,omitempty"`
	Bytes      int64              `json:"bytes,omitempty"`
	Frames     []Frame            `json:"frames,omitempty"`
}

// Frame is one entry of a video frame sequence.
type Frame struct {
	Seq      int64  `json:"seq"`
	PTS      int64  `json:"pts"`
	Keyframe bool   `json:"keyframe"`
	File     string `json:"file"`
	SHA256   string `json:"sha256"`
	Bytes    int64  `json:"bytes"`
}

// Decode strictly parses a manifest: unknown fields are rejected.
func Decode(r io.Reader) (*Manifest, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest decode: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks required fields, uniqueness and value domains.
func (m *Manifest) Validate() error {
	if m.Version != 1 {
		return fmt.Errorf("manifest: unsupported version %d", m.Version)
	}
	if _, err := timeline.New(m.Timebase); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	if m.ColorPolicy != "srgb" {
		return fmt.Errorf("manifest: unsupported color policy %q", m.ColorPolicy)
	}
	if len(m.Tracks) == 0 {
		return fmt.Errorf("manifest: no tracks")
	}
	seen := map[string]bool{}
	for i := range m.Tracks {
		t := &m.Tracks[i]
		if t.ID == "" {
			return fmt.Errorf("manifest: track %d: empty id", i)
		}
		if seen[t.ID] {
			return fmt.Errorf("manifest: duplicate track id %q", t.ID)
		}
		seen[t.ID] = true
		switch t.Kind {
		case "video":
			if err := validateVideo(t); err != nil {
				return fmt.Errorf("manifest: track %q: %w", t.ID, err)
			}
		case "audio":
			if err := validateAudio(t); err != nil {
				return fmt.Errorf("manifest: track %q: %w", t.ID, err)
			}
		default:
			return fmt.Errorf("manifest: track %q: unknown kind %q", t.ID, t.Kind)
		}
	}
	return nil
}

func validateVideo(t *Track) error {
	if t.Codec != "png" && t.Codec != "jpeg" {
		return fmt.Errorf("unknown codec %q", t.Codec)
	}
	if t.FPS == nil {
		return fmt.Errorf("missing fps")
	}
	if t.ColorTag == "" {
		return fmt.Errorf("missing color_tag")
	}
	if len(t.Frames) == 0 {
		return fmt.Errorf("no frames")
	}
	seqs := map[int64]bool{}
	for _, f := range t.Frames {
		if seqs[f.Seq] {
			return fmt.Errorf("duplicate frame seq %d", f.Seq)
		}
		seqs[f.Seq] = true
		if f.File == "" {
			return fmt.Errorf("frame %d: missing file", f.Seq)
		}
		if err := checkSHA(f.SHA256); err != nil {
			return fmt.Errorf("frame %d: %w", f.Seq, err)
		}
		if f.Bytes < 0 {
			return fmt.Errorf("frame %d: negative byte count", f.Seq)
		}
	}
	return nil
}

func validateAudio(t *Track) error {
	if t.Layout == "" {
		return fmt.Errorf("missing layout")
	}
	if t.SampleRate <= 0 {
		return fmt.Errorf("invalid sample_rate %d", t.SampleRate)
	}
	if t.Format != "s16le" {
		return fmt.Errorf("unsupported pcm format %q", t.Format)
	}
	if t.File == "" {
		return fmt.Errorf("missing file")
	}
	return checkSHA(t.SHA256)
}

func checkSHA(s string) error {
	if len(s) != 64 {
		return fmt.Errorf("sha256 must be 64 hex chars, got %d", len(s))
	}
	for _, c := range s {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return fmt.Errorf("sha256 contains non-hex character %q", c)
		}
	}
	return nil
}
