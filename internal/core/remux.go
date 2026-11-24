package core

import (
	"crypto/sha256"
	"fmt"

	mp4adapter "media-check/internal/adapter/mp4"
	"media-check/internal/manifest"
	"media-check/internal/model"
)

// RemuxResult is the output of remuxing one valid segment.
type RemuxResult struct {
	Segment     int
	Data        []byte // output MP4
	Frames      int
	Range       model.TimeRange
	DstHash     [32]byte // hash of frames rebuilt from the remuxed output
	RoundtripOK bool     // re-parsed output reproduces every source payload hash
}

// RemuxSegment repackages one segment's frames into the MP4 container
// via the versioned adapter, then re-parses the output and verifies that
// every sample payload hash matches the source. No re-encoding is done;
// payloads are copied byte-for-byte.
func RemuxSegment(a mp4adapter.Adapter, frames []model.Frame, segIdx int, delta uint32) (RemuxResult, error) {
	res := RemuxResult{Segment: segIdx, Frames: len(frames)}
	if len(frames) == 0 {
		return res, fmt.Errorf("segment %d: no frames to remux", segIdx)
	}
	res.Range = model.TimeRange{Start: frames[0].Time, End: frames[len(frames)-1].Time + int64(delta)}

	samples := make([][]byte, len(frames))
	var keys []uint32
	for i, f := range frames {
		samples[i] = f.Payload
		if f.Keyframe {
			keys = append(keys, uint32(i+1))
		}
	}
	data, err := a.Marshal(samples, keys, model.RTPTimebase, delta)
	if err != nil {
		return res, fmt.Errorf("segment %d: marshal: %w", segIdx, err)
	}
	res.Data = data

	parsed := a.Parse(data, segIdx, res.Range)
	if !parsed.Complete {
		return res, fmt.Errorf("segment %d: re-parse of own output failed: %v", segIdx, parsed.Faults)
	}
	got, err := parsed.Table.Samples(data)
	if err != nil {
		return res, fmt.Errorf("segment %d: sample extraction: %w", segIdx, err)
	}
	if len(got) != len(frames) {
		return res, fmt.Errorf("segment %d: sample count %d != %d", segIdx, len(got), len(frames))
	}
	res.RoundtripOK = true
	for i, s := range got {
		if sha256.Sum256(s) != frames[i].Hash {
			res.RoundtripOK = false
			return res, fmt.Errorf("segment %d: frame %d payload changed across remux", segIdx, i)
		}
	}
	// DstHash is computed from the samples actually extracted back out
	// of the remuxed container, not copied from the source.
	out := make([]model.Frame, len(frames))
	for i, f := range frames {
		out[i] = model.Frame{Seq: f.Seq, Time: f.Time, Keyframe: f.Keyframe, Payload: got[i]}
	}
	res.DstHash = manifest.HashFrames(out)
	return res, nil
}
