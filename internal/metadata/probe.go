package metadata

import (
	"bytes"
	"fmt"
	"io"

	mp4 "github.com/abema/go-mp4"
)

// StreamSummary is the normalized metadata the pipeline consumes; it hides
// go-mp4's box types behind plain fields.
type StreamSummary struct {
	MajorBrand  string
	Timescale   uint32
	Duration    uint64
	FastStart   bool
	VideoTracks []VideoTrackMeta
}

// VideoTrackMeta captures the fields needed to validate color/size metadata.
type VideoTrackMeta struct {
	TrackID   uint32
	Timescale uint32
	Duration  uint64
	Width     uint16
	Height    uint16
	Profile   byte
	Level     byte
}

// Probe parses MP4 metadata through go-mp4 and returns a normalized summary.
// The source must be seekable; bytes are never sent off-device.
func Probe(r io.ReadSeeker) (*StreamSummary, error) {
	info, err := mp4.Probe(r)
	if err != nil {
		return nil, fmt.Errorf("mp4 probe: %w", err)
	}
	if len(info.Tracks) == 0 && info.Timescale == 0 && info.MajorBrand == [4]byte{} {
		return nil, fmt.Errorf("mp4 probe: no recognizable boxes in source")
	}
	out := &StreamSummary{
		MajorBrand: bytes.NewBuffer(info.MajorBrand[:]).String(),
		Timescale:  info.Timescale,
		Duration:   info.Duration,
		FastStart:  info.FastStart,
	}
	for _, tr := range info.Tracks {
		if tr.Codec != mp4.CodecAVC1 || tr.AVC == nil {
			continue
		}
		out.VideoTracks = append(out.VideoTracks, VideoTrackMeta{
			TrackID:   tr.TrackID,
			Timescale: tr.Timescale,
			Duration:  tr.Duration,
			Width:     tr.AVC.Width,
			Height:    tr.AVC.Height,
			Profile:   tr.AVC.Profile,
			Level:     tr.AVC.Level,
		})
	}
	return out, nil
}
