package fixture

import (
	"fmt"
	"os"
	"path/filepath"
)

// Generate writes every case into dir, one subdirectory per case.
func Generate(opts Options) ([]Batch, error) {
	if opts.Segments <= 0 || opts.Segments > 10000 {
		return nil, fmt.Errorf("fixture: segment count %d out of range", opts.Segments)
	}
	if opts.SampleRate != 48000 {
		return nil, fmt.Errorf("fixture: sample rate must be 48000")
	}
	if opts.SegmentTicks <= 0 {
		return nil, fmt.Errorf("fixture: segment ticks must be positive")
	}
	abs, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	batches := make([]Batch, 0, len(AllCases))
	for _, c := range AllCases {
		b, err := generateCase(abs, opts, c)
		if err != nil {
			return nil, err
		}
		if err := b.write(); err != nil {
			return nil, err
		}
		batches = append(batches, *b)
	}
	return batches, nil
}

func generateCase(root string, opts Options, c Case) (*Batch, error) {
	dir := filepath.Join(root, string(c))
	segDur := opts.SegmentTicks
	initBytes := buildInit(opts.SampleRate, 1)
	b := &Batch{Case: c, Dir: dir, SampleRate: opts.SampleRate, InitBytes: initBytes, HasShippedIndex: c != CaseMissingIdx}
	rng := newRNG(opts.Seed + int64(len(c)))
	for i := 0; i < opts.Segments; i++ {
		uri := fmt.Sprintf("seg%03d.m4s", i)
		start := int64(i) * segDur
		seq := uint64(i + 1)
		keyframe := i%3 == 0
		data := buildMedia(start, segDur, opts.SampleRate, seq, keyframe)
		seg := Segment{URI: uri, Sequence: seq, StartTicks: start, Duration: segDur, Bytes: data}
		switch c {
		case CaseTruncated:
			if i == opts.Segments/2 {
				cut := len(data) / 2
				seg.Bytes = append([]byte(nil), data[:cut]...)
				seg.Truncated = true
			}
		case CaseCorruptBox:
			if i == opts.Segments/3 {
				seg.Bytes = corruptSecondBox(data, rng)
				seg.Corrupt = true
			}
		case CaseTimestamp:
			// Shift every segment from the midpoint onward so the media
			// timeline jumps once and then stays contiguous. The playlist
			// still advertises the original positions, which is exactly the
			// cross-segment discontinuity the probe must report.
			if i >= opts.Segments/2 {
				seg.StartTicks = start + segDur
				seg.Bytes = buildMedia(seg.StartTicks, segDur, opts.SampleRate, seq, keyframe)
				seg.TimeJump = i == opts.Segments/2
			}
		}
		b.Segments = append(b.Segments, seg)
		if keyframe {
			b.KeyframeTicks = append(b.KeyframeTicks, seg.StartTicks)
		}
	}
	hls, err := buildHLS(b)
	if err != nil {
		return nil, err
	}
	dash, err := buildDASH(b, opts)
	if err != nil {
		return nil, err
	}
	b.HLS, b.DASH = hls, dash
	return b, nil
}

func (b *Batch) write() error {
	if err := os.MkdirAll(b.Dir, 0o755); err != nil {
		return err
	}
	for _, seg := range b.Segments {
		path, err := SafeJoin(b.Dir, seg.URI)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, seg.Bytes, 0o644); err != nil {
			return err
		}
	}
	initPath, err := SafeJoin(b.Dir, InitName)
	if err != nil {
		return err
	}
	if err := os.WriteFile(initPath, b.InitBytes, 0o644); err != nil {
		return err
	}
	if err := writeSafe(b.Dir, ManifestHLS, b.HLS); err != nil {
		return err
	}
	if err := writeSafe(b.Dir, ManifestDASH, b.DASH); err != nil {
		return err
	}
	return b.WriteIndex()
}
