// Package fixture generates deterministic sample media so every test and demo
// runs from code alone, with no external files and no network access.
package fixture

import (
	"math/rand"

	"mediamix/internal/core"
	"mediamix/internal/timeline"
)

// Limits caps generated work so a hostile seed or flag cannot request an
// unbounded fixture.
type Limits struct {
	MaxTracks  int
	MaxPackets int
	MaxSeconds int
	MaxPayload int
}

// DefaultLimits bound a default demo run.
func DefaultLimits() Limits {
	return Limits{MaxTracks: 32, MaxPackets: 20000, MaxSeconds: 120, MaxPayload: 1200}
}

// Validate rejects out-of-range limits.
func (l Limits) Validate() error {
	if l.MaxTracks <= 0 || l.MaxTracks > 256 {
		return &core.Error{Code: core.CodeOverLimit, Detail: "fixture track limit outside 1..256", Fatal: true}
	}
	if l.MaxPackets <= 0 || l.MaxPackets > 1_000_000 {
		return &core.Error{Code: core.CodeOverLimit, Detail: "fixture packet limit outside 1..1e6", Fatal: true}
	}
	if l.MaxSeconds <= 0 || l.MaxSeconds > 3600 {
		return &core.Error{Code: core.CodeOverLimit, Detail: "fixture duration limit outside 1..3600s", Fatal: true}
	}
	if l.MaxPayload <= 0 || l.MaxPayload > 65000 {
		return &core.Error{Code: core.CodeOverLimit, Detail: "fixture payload limit outside 1..65535", Fatal: true}
	}
	return nil
}

// Params selects the fixture variant.
type Params struct {
	Seed          int64
	LossBurst     int
	BandwidthMBps float64
	CorruptIndex  bool
	TruncateTrack string
	Limits        Limits
}

// TrackSpec describes one generated track.
type TrackSpec struct {
	ID          string
	Kind        timeline.NodeKind
	Format      string
	Timescale   uint32
	Origin      timeline.Micros
	DurationUs  timeline.Micros
	PacketCount int
	// SegmentLen is the nominal spacing between packets in microseconds.
	SegmentLen timeline.Micros
	Audio      *AudioSpec
}

// AudioSpec describes generated PCM material.
type AudioSpec struct {
	Rate      int
	Layout    string
	FreqHz    float64
	Amplitude float64
	Silent    bool
}

// SentPacket is one packet the sender transmitted, before transport effects.
type SentPacket struct {
	Track     string
	Seq       uint16
	Timestamp uint32
	Marker    bool
	Keyframe  bool
	Payload   []byte
	SentUs    timeline.Micros
}

// Keyframe is a generated index entry.
type Keyframe struct {
	OffsetUs timeline.Micros
	Size     int
	PTS      timeline.Micros
	Key      bool
}

// Fixture is a full generated scenario.
type Fixture struct {
	Params  Params
	Tracks  []TrackSpec
	Streams map[string][]byte
	Indexes map[string][]Keyframe
	Sent    map[string][]SentPacket
	Audio   map[string]*Material
	// Corrupt records the deliberate defect applied to a track, if any.
	Corrupt map[string]string
}

// Material re-exports audio material shape without importing the audio package
// (avoids a dependency cycle in tests that construct fixtures).
type Material struct {
	Track   string
	Rate    int
	Layout  string
	Span    timeline.Span
	Samples []float64
}

// defaultTimescales per codec keep timestamps realistic.
var defaultTimescales = map[string]uint32{
	"h264": 90000,
	"h265": 90000,
	"av1":  90000,
}

// Generate builds a deterministic fixture from params. Identical params always
// produce identical bytes, which makes failures reproducible.
func Generate(p Params) (*Fixture, error) {
	if p.Limits == (Limits{}) {
		p.Limits = DefaultLimits()
	}
	if err := p.Limits.Validate(); err != nil {
		return nil, err
	}
	if p.LossBurst < 0 {
		return nil, &core.Error{Code: core.CodeBadArgument, Detail: "loss burst must be >= 0", Fatal: true}
	}
	if p.BandwidthMBps < 0 {
		return nil, &core.Error{Code: core.CodeBadArgument, Detail: "bandwidth must be >= 0", Fatal: true}
	}
	rng := rand.New(rand.NewSource(p.Seed))
	f := &Fixture{
		Params:  p,
		Streams: make(map[string][]byte),
		Indexes: make(map[string][]Keyframe),
		Sent:    make(map[string][]SentPacket),
		Audio:   make(map[string]*Material),
		Corrupt: make(map[string]string),
	}
	f.Tracks = specs(p)
	if len(f.Tracks) > p.Limits.MaxTracks {
		return nil, &core.Error{Code: core.CodeOverLimit, Detail: "fixture requests too many tracks", Fatal: true}
	}
	maxDuration := timeline.Micros(p.Limits.MaxSeconds) * timeline.MicrosPerSecond
	total := 0
	for _, spec := range f.Tracks {
		// Single-track duration is capped separately from the total packet
		// count: one long track is as much a denial-of-service risk as many.
		if spec.DurationUs > maxDuration {
			return nil, &core.Error{Code: core.CodeOverLimit, Track: spec.ID,
				Detail: "track duration exceeds the per-track limit", Fatal: true}
		}
		if err := f.buildTrack(spec, rng); err != nil {
			return nil, err
		}
		total += len(f.Sent[spec.ID])
		if total > p.Limits.MaxPackets {
			return nil, &core.Error{Code: core.CodeOverLimit, Detail: "fixture requests too many packets", Fatal: true}
		}
		for _, pkt := range f.Sent[spec.ID] {
			if len(pkt.Payload) > p.Limits.MaxPayload {
				return nil, &core.Error{Code: core.CodeOverLimit, Track: spec.ID,
					Detail: "packet payload exceeds the per-packet limit", Fatal: true}
			}
		}
	}
	if p.TruncateTrack != "" {
		if err := f.truncate(p.TruncateTrack); err != nil {
			return nil, err
		}
	}
	if p.CorruptIndex {
		f.corruptIndex(rng)
	}
	return f, nil
}

// specs returns the fixed track layout for a demo: two video angles, three
// audio sources including one that is intentionally empty (audio-empty) and a
// clipped one (audio-3) so status reporting is exercised.
func specs(p Params) []TrackSpec {
	sec := func(n int) timeline.Micros { return timeline.Micros(n) * timeline.MicrosPerSecond }
	return []TrackSpec{
		{
			ID: "cam-a", Kind: timeline.KindVideo, Format: "h264", Timescale: defaultTimescales["h264"],
			Origin: 0, DurationUs: sec(6), PacketCount: 180, SegmentLen: 33333,
		},
		{
			ID: "cam-b", Kind: timeline.KindVideo, Format: "h265", Timescale: defaultTimescales["h265"],
			Origin: 12000, DurationUs: sec(6), PacketCount: 180, SegmentLen: 33333,
		},
		{
			ID: "proxy-av1", Kind: timeline.KindVideo, Format: "av1", Timescale: defaultTimescales["av1"],
			Origin: 0, DurationUs: sec(6), PacketCount: 120, SegmentLen: 50000,
		},
		{
			ID: "audio-1", Kind: timeline.KindAudio, Format: "pcm", Timescale: 48000,
			Origin: 0, DurationUs: sec(6), PacketCount: 90, SegmentLen: 66666,
			Audio: &AudioSpec{Rate: 48000, Layout: "stereo", FreqHz: 440, Amplitude: 0.35},
		},
		{
			ID: "audio-2", Kind: timeline.KindAudio, Format: "pcm", Timescale: 44100,
			Origin: 0, DurationUs: sec(6), PacketCount: 90, SegmentLen: 66666,
			Audio: &AudioSpec{Rate: 44100, Layout: "mono", FreqHz: 220, Amplitude: 0.2},
		},
		{
			ID: "audio-empty", Kind: timeline.KindAudio, Format: "pcm", Timescale: 48000,
			Origin: 0, DurationUs: 0, PacketCount: 10, SegmentLen: 66666,
			Audio: &AudioSpec{Rate: 48000, Layout: "stereo", Silent: true},
		},
	}
}
