package ingest

import (
	"fmt"

	"mediacheck/internal/clock"
	"mediacheck/internal/model"
)

// Generate builds a deterministic, decodable fixture using the real libopus
// and libx264 encoders, then replays it into the unified packet event model.
// Equal seeds and configs produce byte-identical payloads; corruption is a
// separate explicit step (see ApplyProfile) so each fault is isolated.
func Generate(cfg Config) (*Generated, error) {
	cfg, err := cfg.Validate()
	if err != nil {
		return nil, err
	}
	assets, err := encodeFixture(cfg)
	if err != nil {
		return nil, err
	}
	return assemble(cfg, assets)
}

// assemble turns encoded assets into stream descriptors and packets with
// exact tick timestamps and nanosecond slots.
func assemble(cfg Config, assets *mediaAssets) (*Generated, error) {
	g := &Generated{Config: cfg}
	videoTB := clock.Video(VideoFPSNum, VideoFPSDen)
	audioTB := clock.Audio(AudioSampleRate)

	video := &Stream{Track: model.TrackVideo, Timebase: videoTB}
	for i, v := range assets.Video {
		start := videoTB.Nanos(int64(i))
		end := videoTB.Nanos(int64(i + 1))
		key := v.Keyframe || i%cfg.KeyframeInterval == 0
		video.Packets = append(video.Packets, model.Packet{
			Track: model.TrackVideo, Codec: model.CodecH264,
			Sequence: uint32(i), PTS: int64(i), DTS: int64(i), Duration: 1,
			Keyframe: key,
			Data:     append([]byte(nil), v.Data...),
			Timebase: videoTB,
			Slot:     model.Slot{Start: start, End: end},
		})
		g.VideoSlots = append(g.VideoSlots, start)
	}
	video.Sequence = uint32(len(video.Packets))

	audio := &Stream{Track: model.TrackAudio, Timebase: audioTB}
	cursor := 0
	for i, a := range assets.Audio {
		start := audioTB.Nanos(int64(cursor))
		end := audioTB.Nanos(int64(cursor + a.Samples))
		audio.Packets = append(audio.Packets, model.Packet{
			Track: model.TrackAudio, Codec: g.Config.Codec,
			Sequence: uint32(i), PTS: int64(cursor), DTS: int64(cursor), Duration: int64(a.Samples),
			Keyframe: true,
			Data:     append([]byte(nil), a.Data...),
			Timebase: audioTB,
			Slot:     model.Slot{Start: start, End: end},
		})
		g.AudioSlots = append(g.AudioSlots, model.Slot{Start: start, End: end})
		cursor += a.Samples
	}
	audio.Sequence = uint32(len(audio.Packets))

	g.Audio = audio
	g.Video = video
	g.AudioTotal = len(audio.Packets)
	g.VideoTotal = len(video.Packets)
	g.Assets = assets
	g.Span = audioTB.Nanos(int64(cursor))
	return g, nil
}

// Fixture flattens the generated streams into a single event fixture with
// stream descriptors.
func (g *Generated) Fixture() *model.Fixture {
	f := &model.Fixture{}
	for _, s := range []*Stream{g.Audio, g.Video} {
		if s == nil || len(s.Packets) == 0 {
			continue
		}
		info := model.StreamInfo{
			Track:    s.Track,
			Timebase: s.Timebase,
		}
		switch s.Track {
		case model.TrackAudio:
			info.Codec = g.Config.Codec
			info.SampleRate = AudioSampleRate
			info.Channels = AudioChannels
			info.Extradata = g.Assets.AudioExtra
		case model.TrackVideo:
			info.Codec = model.CodecH264
			info.Width = VideoWidth
			info.Height = VideoHeight
			info.FrameRate = s.Timebase
			info.Extradata = g.Assets.VideoExtra
		}
		f.Streams = append(f.Streams, info)
		f.Packets = append(f.Packets, s.Packets...)
	}
	return f
}

// Summary renders the fixture shape for the demo header.
func (g *Generated) Summary() string {
	return fmt.Sprintf("audio=%d video=%d span=%.3fs", g.AudioTotal, g.VideoTotal, float64(g.Span)/1e9)
}
