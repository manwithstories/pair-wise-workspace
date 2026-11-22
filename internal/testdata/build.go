package testdata

import (
	"time"

	"github.com/pion/rtp"

	"mediacq/internal/clock"
)

// BuildRTP packetizes the video track into RTP packets on a 32-bit timestamp
// space. WrapAt forces the series close to the 2^32 boundary so at least one
// real wrap occurs; DriftPPM skews sender ticks relative to the reference.
func BuildRTP(video TrackSpec, p Params) []RTPPacket {
	out := make([]RTPPacket, 0, len(video.Samples))
	var raw uint32
	switch {
	case p.WrapAt > 0:
		raw = p.WrapAt
	default:
		raw = 1000
	}
	var tickPos int64
	for i, s := range video.Samples {
		// First packet sits exactly on raw; subsequent packets advance by the
		// accumulated sample duration, so a near-ceiling start rolls over once.
		skewed := applyDrift(tickPos, p.DriftPPM)
		ts := uint32(int64(raw) + skewed)
		pkt := RTPPacket{
			Timestamp:   ts,
			Sequence:    uint16(i + 1),
			SSRC:        0x4d435131,
			Marker:      s.Keyframe,
			PayloadType: 96,
			Payload:     s.Payload,
		}
		out = append(out, pkt)
		tickPos += int64(s.DurationTicks)
	}
	return out
}

// MarshalRTP encodes packets through pion/rtp so ingest tests parse genuine
// RFC 3550 headers rather than in-memory structs.
func MarshalRTP(pkts []RTPPacket) ([][]byte, error) {
	out := make([][]byte, 0, len(pkts))
	for _, p := range pkts {
		hdr := rtp.Header{
			Version:        2,
			PayloadType:    p.PayloadType,
			SequenceNumber: p.Sequence,
			Timestamp:      p.Timestamp,
			SSRC:           p.SSRC,
			Marker:         p.Marker,
		}
		pkt := &rtp.Packet{Header: hdr, Payload: p.Payload}
		b, err := pkt.Marshal()
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// Build assembles the complete deterministic fixture.
func Build(p Params) (*Fixture, error) {
	video := BuildVideo(p)
	audio, silences := BuildAudio(p)
	total, err := video.Rate.Duration(int64(video.DurationTicks()))
	if err != nil {
		return nil, err
	}
	evs := BuildEvents(p, total)
	container, err := WriteMP4(video, audio, evs, p)
	if err != nil {
		return nil, err
	}
	pkts := BuildRTP(video, p)
	keys := make([]int, 0)
	for i, s := range video.Samples {
		if s.Keyframe {
			keys = append(keys, i)
		}
	}
	return &Fixture{
		Params: p, Video: video, Audio: audio, Events: evs,
		MP4: container, RTP: pkts,
		ExpectedSilences: silences, ExpectedKeyframes: keys,
	}, nil
}

// TotalDuration returns the program duration on the unified timeline.
func (f *Fixture) TotalDuration() (time.Duration, error) {
	return f.Video.Rate.Duration(int64(f.Video.DurationTicks()))
}

// VideoRate exposes the video clock for downstream conversion tests.
func (f *Fixture) VideoRate() clock.Rate { return f.Video.Rate }
