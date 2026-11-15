// Package plan defines the persistent segment plan shared by all commands.
package plan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const Version = 1

type Segment struct {
	Index         int     `json:"index"`
	StartFrame    int     `json:"start_frame"`
	EndFrame      int     `json:"end_frame"`
	Frames        int     `json:"frames"`
	TargetKbps    float64 `json:"target_kbps"`
	FirstPassKbps float64 `json:"first_pass_kbps,omitempty"`
	Complexity    float64 `json:"complexity,omitempty"`
	HardCut       bool    `json:"hard_cut"`
}

type Plan struct {
	Version    int       `json:"version"`
	PlanID     string    `json:"plan_id,omitempty"`
	Clip       string    `json:"clip"`
	Frames     int       `json:"frames"`
	Width      int       `json:"width"`
	Height     int       `json:"height"`
	FPS        int       `json:"fps"`
	GOP        int       `json:"gop"`
	TargetKbps float64   `json:"target_kbps"`
	Seed       int64     `json:"seed"`
	Segments   []Segment `json:"segments"`
}

// New creates a GOP-aligned plan with placeholder targets.
func New(clip string, frames, width, height, fps, gop int, targetKbps float64, seed int64) *Plan {
	p := &Plan{Version: Version, Clip: clip, Frames: frames, Width: width, Height: height,
		FPS: fps, GOP: gop, TargetKbps: targetKbps, Seed: seed}
	for start := 0; start < frames; start += gop {
		end := start + gop
		if end > frames {
			end = frames
		}
		p.Segments = append(p.Segments, Segment{
			Index: len(p.Segments), StartFrame: start, EndFrame: end,
			Frames: end - start, TargetKbps: targetKbps,
		})
	}
	return p
}

// Validate rejects structurally inconsistent plans.
func (p *Plan) Validate() error {
	if p.Version != Version {
		return fmt.Errorf("unsupported plan version %d", p.Version)
	}
	if p.Frames < 0 || p.TargetKbps <= 0 || p.FPS <= 0 || p.GOP <= 0 {
		return fmt.Errorf("frames, target, fps, and gop must be positive (frames may be zero)")
	}
	if p.Width < 16 || p.Height < 16 || p.Width%16 != 0 || p.Height%16 != 0 {
		return fmt.Errorf("plan dimensions must be multiples of 16 and at least 16")
	}
	if p.Clip == "" {
		return fmt.Errorf("clip is required")
	}
	if p.Frames == 0 {
		if len(p.Segments) != 0 {
			return fmt.Errorf("zero-frame plan must have zero segments")
		}
		return nil
	}
	if len(p.Segments) == 0 {
		return fmt.Errorf("non-empty plan has no segments")
	}
	next := 0
	for i, s := range p.Segments {
		if s.Index != i {
			return fmt.Errorf("segment %d has index %d", i, s.Index)
		}
		if s.StartFrame != next || s.EndFrame <= s.StartFrame || s.EndFrame > p.Frames {
			return fmt.Errorf("segment %d range [%d,%d) is inconsistent", i, s.StartFrame, s.EndFrame)
		}
		if s.Frames != s.EndFrame-s.StartFrame || s.Frames > p.GOP {
			return fmt.Errorf("segment %d frame count is inconsistent", i)
		}
		if s.TargetKbps <= 0 {
			return fmt.Errorf("segment %d target must be positive", i)
		}
		next = s.EndFrame
	}
	if next != p.Frames {
		return fmt.Errorf("segments cover %d of %d frames", next, p.Frames)
	}
	return nil
}

// ComputeID hashes the semantic plan contents, excluding the ID itself.
func (p *Plan) ComputeID() string {
	clone := *p
	clone.PlanID = ""
	clone.Segments = append([]Segment(nil), p.Segments...)
	b, _ := json.Marshal(clone)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// EnsureID fills a missing ID and verifies an existing one.
func (p *Plan) EnsureID() error {
	id := p.ComputeID()
	if p.PlanID == "" {
		p.PlanID = id
		return nil
	}
	if p.PlanID != id {
		return fmt.Errorf("plan_id does not match plan contents")
	}
	return nil
}

func (p *Plan) Segment(index int) (*Segment, bool) {
	if index < 0 || index >= len(p.Segments) {
		return nil, false
	}
	return &p.Segments[index], true
}

func (p *Plan) SegmentBytes(seg Segment) int64 {
	return int64(seg.TargetKbps * 1000 / float64(p.FPS) / 8 * float64(seg.Frames))
}

func ActualKbps(bytes int64, frames, fps int) float64 {
	if frames <= 0 || fps <= 0 {
		return 0
	}
	return float64(bytes) * 8 * float64(fps) / float64(frames) / 1000
}

func Load(path string) (*Plan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Plan
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("decode plan: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if err := p.EnsureID(); err != nil {
		return nil, err
	}
	return &p, nil
}

func Save(path string, p *Plan) error {
	if err := p.EnsureID(); err != nil {
		return err
	}
	if err := p.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
