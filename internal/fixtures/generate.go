// Package fixtures assembles the demo's on-disk inputs. Keeping generation here
// means the CLI has no external sample dependencies and the demo is byte-for-
// byte reproducible from its seed.
package fixtures

import (
	"fmt"
	"os"
	"path/filepath"
)

// GenerateInputs writes every fixture the demo needs into dir and returns their
// paths. Existing files are overwritten so a rerun is deterministic.
type Generated struct {
	MP4          string
	MXF          string
	RTP          string
	TruncatedMP4 string
	InvalidKLV   string
}

// Generate writes the clean and damaged inputs plus the RTP stream.
func Generate(dir string) (*Generated, error) {
	if dir == "" {
		return nil, fmt.Errorf("fixtures: empty generation directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	out := &Generated{
		MP4:          filepath.Join(dir, "studio.mp4"),
		MXF:          filepath.Join(dir, "field.mxf"),
		RTP:          filepath.Join(dir, "rtp-input.bin"),
		TruncatedMP4: filepath.Join(dir, "truncated.mp4"),
		InvalidKLV:   filepath.Join(dir, "invalid-klv.mxf"),
	}

	mp4, err := BuildMP4(DemoMP4Options())
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(out.MP4, mp4.Data, 0o644); err != nil {
		return nil, err
	}
	mxf, err := BuildMXF(DemoMxfOptions())
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(out.MXF, mxf.Data, 0o644); err != nil {
		return nil, err
	}
	rtp, err := BuildRTP(DefaultRTPStreamOptions())
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(out.RTP, EncodeRTPStream(rtp.Packets), 0o644); err != nil {
		return nil, err
	}
	tr, err := BuildMP4Truncated(DemoMP4Options())
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(out.TruncatedMP4, tr.Data, 0o644); err != nil {
		return nil, err
	}
	bad, err := BuildMXFInvalidKLV(DemoMxfOptions())
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(out.InvalidKLV, bad.Data, 0o644); err != nil {
		return nil, err
	}
	return out, nil
}

// DemoMP4Options sizes the demo MP4 so the program lasts about ten seconds and
// splits into several checkpoint blocks at the default 120 samples per block.
// Sample sizes keep the program near 1950 kbps, inside the 2000 kbps target's
// 10% tolerance.
func DemoMP4Options() Options {
	o := DefaultOptions()
	o.VideoFrames = 300
	o.AudioSamples = 480
	return o
}

// DemoMxfOptions matches the demo duration for the MXF input.
func DemoMxfOptions() BuildMxfOptions {
	o := DefaultMxfOptions()
	o.VideoSamples = 300
	o.AudioSamples = 480
	return o
}

// DefaultRTPStreamOptions returns the impairment set the demo exposes as flags,
// with the flag defaults applied.
func DefaultRTPStreamOptions() RTPOptions {
	o := DefaultRTPOptions()
	o.LossBurst = 7
	o.Duplicate = 3
	o.Reorder = 5
	return o
}

// RTPOptionsFromFlags builds the stream options from demo flags.
func RTPOptionsFromFlags(seed int64, lossBurst, duplicate, reorder int) RTPOptions {
	o := DefaultRTPOptions()
	o.Seed = seed
	o.LossBurst = lossBurst
	o.Duplicate = duplicate
	o.Reorder = reorder
	return o
}
