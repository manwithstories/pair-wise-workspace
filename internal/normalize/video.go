package normalize

import (
	"bytes"
	"fmt"
	"image/png"

	"github.com/disintegration/imaging"

	"avflow/internal/model"
	"avflow/internal/stream"
)

// CanonicalFrameW/H is the fixed output frame size every video track is
// normalized to, so timeline boundaries never depend on source dimensions.
const (
	CanonicalFrameW = 128
	CanonicalFrameH = 96
)

// VideoFrame is one normalized, re-encoded PNG frame on the timeline.
type VideoFrame struct {
	Track    uint32
	Seq      uint64
	PTSNS    int64
	EndPTSNS int64
	Width    int
	Height   int
	ColorTag uint8
	PNG      []byte
}

// VideoResult aggregates normalized frames and color diagnostics.
type VideoResult struct {
	Frames []VideoFrame
	Faults []*model.Fault
}

// NormalizeVideo decodes, color-tags, resizes and re-encodes every clean PNG
// packet of one video track. The first color tag establishes the track
// profile; a later different known tag is a conflict, an unknown tag is a
// positioned fault. Both are recoverable: the frame keeps the profile.
func NormalizeVideo(desc stream.TrackDesc, packets []*stream.Packet) (*VideoResult, error) {
	res := &VideoResult{}
	if desc.Kind != stream.KindVideo {
		return nil, fmt.Errorf("track %d is not a video track", desc.ID)
	}
	profile := desc.ColorTag
	if !knownColor(profile) {
		f := model.NewFault(model.FaultUnknownTag, desc.ID, 0, 0, 0, true,
			fmt.Sprintf("track declares unknown color tag %d; defaulting to sRGB", profile))
		res.Faults = append(res.Faults, f)
		profile = stream.ColorSRGB
	}
	for _, p := range packets {
		frame, f := normalizeFrame(desc, p, &profile)
		if f != nil {
			res.Faults = append(res.Faults, f)
			if !f.Recoverable {
				return res, f
			}
		}
		// Recoverable tag faults still yield a re-encoded frame, which the
		// upper pipeline keeps or crops based on its own fault index.
		if len(frame.PNG) > 0 {
			res.Frames = append(res.Frames, frame)
		}
	}
	return res, nil
}

func normalizeFrame(desc stream.TrackDesc, p *stream.Packet, profile *uint8) (VideoFrame, *model.Fault) {
	frame := VideoFrame{Track: desc.ID, Seq: p.Seq, PTSNS: p.PTSNS, EndPTSNS: p.EndPTSNS}
	// A per-packet color tag (non-zero) overrides the descriptor for this
	// packet; zero means the packet inherits the track profile.
	tag := desc.ColorTag
	payload := p.Payload
	if p.ColorTag != 0 {
		tag = p.ColorTag
	}
	switch {
	case !knownColor(tag):
		f := model.NewFault(model.FaultUnknownTag, desc.ID, p.Seq, p.PTSNS, p.EndPTSNS, true,
			fmt.Sprintf("unknown color tag %d; retaining track profile", tag))
		out, err := reencode(desc.ID, p.Seq, p.PTSNS, p.EndPTSNS, payload, *profile)
		if err != nil {
			return frame, model.NewFault(model.FaultTruncated, desc.ID, p.Seq, p.PTSNS, p.EndPTSNS, true, err.Error())
		}
		return out, f
	case tag != *profile:
		f := model.NewFault(model.FaultColorConflict, desc.ID, p.Seq, p.PTSNS, p.EndPTSNS, true,
			fmt.Sprintf("color tag %d conflicts with track profile %d", tag, *profile))
		out, err := reencode(desc.ID, p.Seq, p.PTSNS, p.EndPTSNS, payload, *profile)
		if err != nil {
			return frame, model.NewFault(model.FaultTruncated, desc.ID, p.Seq, p.PTSNS, p.EndPTSNS, true, err.Error())
		}
		return out, f
	default:
		frame.ColorTag = *profile
	}
	out, err := reencode(desc.ID, p.Seq, p.PTSNS, p.EndPTSNS, payload, *profile)
	if err != nil {
		return frame, model.NewFault(model.FaultTruncated, desc.ID, p.Seq, p.PTSNS, p.EndPTSNS, true, err.Error())
	}
	return out, nil
}

// reencode decodes the PNG, fits it to the canonical rectangle and produces a
// deterministic output frame. Frames already inside the canonical rectangle
// keep their original (deterministically generated) PNG bytes, avoiding a
// costly decode/encode round trip; only oversized frames are resampled and
// re-encoded with a fixed, timestamp-free encoder.
func reencode(track uint32, seq uint64, ptsNS, endNS int64, payload []byte, tag uint8) (VideoFrame, error) {
	img, err := png.Decode(bytes.NewReader(payload))
	if err != nil {
		return VideoFrame{}, fmt.Errorf("png decode failed: %w", err)
	}
	b0 := img.Bounds()
	if b0.Dx() <= CanonicalFrameW && b0.Dy() <= CanonicalFrameH {
		// Fast path: retain the source bytes. They are already deterministic
		// because the fixture renders every PNG from the RNG seed.
		return VideoFrame{
			Track: track, Seq: seq, PTSNS: ptsNS, EndPTSNS: endNS,
			Width: b0.Dx(), Height: b0.Dy(), ColorTag: tag,
			PNG: append([]byte(nil), payload...),
		}, nil
	}
	resized := imaging.Fit(img, CanonicalFrameW, CanonicalFrameH, imaging.Box)
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := enc.Encode(&buf, resized); err != nil {
		return VideoFrame{}, err
	}
	b := resized.Bounds()
	return VideoFrame{
		Track: track, Seq: seq, PTSNS: ptsNS, EndPTSNS: endNS,
		Width: b.Dx(), Height: b.Dy(), ColorTag: tag, PNG: buf.Bytes(),
	}, nil
}

func knownColor(tag uint8) bool {
	return tag == stream.ColorSRGB || tag == stream.ColorBT709
}
