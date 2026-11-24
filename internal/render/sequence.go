package render

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image/png"

	"mediarun/internal/clock"
	"mediarun/internal/model"
)

// Render draws one raster per keyframe-ish review point. Review points are
// the distinct start ticks of frame events plus every metadata event, capped
// at MaxFrames. The image content is a stable function of the folded events,
// so the same input always produces the same PNG bytes.
func Render(events []model.Event, cfg Config) (*Sequence, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	points := reviewPoints(events)
	if len(points) == 0 {
		return nil, fmt.Errorf("%w: no review points", model.ErrInvalid)
	}
	if len(points) > MaxFrames {
		points = points[:MaxFrames]
	}
	seq := &Sequence{Width: cfg.Width, Height: cfg.Height}
	byStart := map[clock.Tick][]model.Event{}
	for _, ev := range events {
		byStart[ev.Start] = append(byStart[ev.Start], ev)
	}
	for i, point := range points {
		folded := byStart[point.start]
		img := draw(cfg, point.start, folded)
		var buf bytes.Buffer
		enc := png.Encoder{CompressionLevel: png.BestSpeed}
		if err := enc.Encode(&buf, img); err != nil {
			return nil, err
		}
		sum := sha256.Sum256(buf.Bytes())
		frame := Frame{
			Index:   i + 1,
			TrackID: point.track,
			Start:   point.start,
			Kind:    point.kind,
			Label:   fmt.Sprintf("%06d-%03d", point.start, i+1),
			PNG:     buf.Bytes(),
			SHA256:  hex.EncodeToString(sum[:]),
		}
		for _, ev := range folded {
			frame.Events = append(frame.Events, ev.Digest())
		}
		seq.Frames = append(seq.Frames, frame)
	}
	manifest, err := seq.buildManifest()
	if err != nil {
		return nil, err
	}
	seq.Manifest = manifest
	return seq, nil
}
