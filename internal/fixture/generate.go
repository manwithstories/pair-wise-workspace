// Package fixture generates valid and corrupted local MP4/MOV test material.
package fixture

import (
	"encoding/json"
	"fmt"
	"time"

	"mediad/internal/media"
)

// Config controls deterministic fixture generation.
type Config struct {
	Seed     int64
	Duration time.Duration
}

// VideoRate and AudioRate are the mobile demo clocks.
const (
	VideoTimescale = 90_000
	VideoFrameDur  = 3000 // 30 fps
	AudioTimescale = 48_000
	AudioFrameDur  = 1024 // AAC 1024 samples per packet
	KeyframeEvery  = 30
)

// BaseProject builds a valid 60-second-style project without external assets.
func BaseProject(seed int64, duration time.Duration) *media.Project {
	seconds := int64(duration.Seconds())
	if seconds < 1 {
		seconds = 60
	}
	videoFrames := int(seconds * 30)
	audioFrames := int((seconds*AudioTimescale + AudioFrameDur - 1) / AudioFrameDur)

	video := &media.Track{
		ID: 1, Kind: media.KindVideo, Timescale: VideoTimescale,
		Width: 1280, Height: 720, BitrateKbps: 2500,
		Color: &media.ColorMetadata{Primaries: "bt709", Transfer: "bt709", Matrix: "bt709", BitDepth: 8},
	}
	for i := 0; i < videoFrames; i++ {
		sync := i%KeyframeEvery == 0
		video.Samples = append(video.Samples, media.Sample{
			Index:    uint32(i + 1),
			PTS:      int64(i) * VideoFrameDur,
			DTS:      int64(i) * VideoFrameDur,
			Duration: VideoFrameDur,
			Size:     uint32(48 + ((i*17 + int(seed)) % 96)),
			Sync:     sync,
		})
	}

	audio := &media.Track{
		ID: 2, Kind: media.KindAudio, Timescale: AudioTimescale, BitrateKbps: 128,
		Audio: &media.AudioMetadata{
			SampleRate: AudioTimescale, ChannelCount: 2,
			Layout: []string{"L", "R"}, Source: "fixture",
		},
	}
	for i := 0; i < audioFrames; i++ {
		audio.Samples = append(audio.Samples, media.Sample{
			Index:    uint32(i + 1),
			PTS:      int64(i) * AudioFrameDur,
			DTS:      int64(i) * AudioFrameDur,
			Duration: AudioFrameDur,
			Size:     24,
			Sync:     true,
		})
	}

	return &media.Project{
		MovieTimescale: VideoTimescale,
		DurationTicks:  uint64(seconds * VideoTimescale),
		Tracks:         []*media.Track{video, audio},
		SourceFormat:   "mp4",
	}
}

// WithBackwardTimestamp corrupts composition timing at a deterministic sample.
func WithBackwardTimestamp(p *media.Project, sample int) *media.Project {
	cp := cloneProject(p)
	v := cp.Tracks[0]
	if sample < 2 || sample >= len(v.Samples) {
		sample = KeyframeEvery + 7
	}
	v.Samples[sample].PTS = v.Samples[sample-1].PTS - 900
	// Also create a missing stts delta later so repair demonstrates both fixes.
	missing := sample + 5
	if missing < len(v.Samples) {
		v.Samples[missing].Duration = 0
	}
	return cp
}

// WithAudioConflict creates rate and channel-map conflicts with corroborating
// alternate declarations, so validation can preserve and repair both values.
func WithAudioConflict(p *media.Project) *media.Project {
	cp := cloneProject(p)
	a := cp.Tracks[1].Audio
	a.DeclaredRate = AudioTimescale
	a.DeclaredChannels = 2
	a.SampleRate = 22_050
	a.ChannelCount = 1
	a.Layout = []string{"L", "R"}
	return cp
}

// WithAmbiguousAudioConflict creates an unrepairable layout conflict for tests.
func WithAmbiguousAudioConflict(p *media.Project) *media.Project {
	cp := cloneProject(p)
	a := cp.Tracks[1].Audio
	a.SampleRate = 44_100
	a.ChannelCount = 2
	a.Layout = []string{"L", "R", "C"}
	return cp
}

// WithColorConflict creates a resolution-backed family mismatch.
func WithColorConflict(p *media.Project) *media.Project {
	cp := cloneProject(p)
	c := cp.Tracks[0].Color
	c.Primaries = "bt709"
	c.Matrix = "bt2020nc"
	c.ConflictDetail = "colr=709 matrix=2020"
	return cp
}

func cloneProject(p *media.Project) *media.Project {
	data, err := json.Marshal(p)
	if err != nil {
		panic(fmt.Sprintf("clone fixture project: %v", err))
	}
	var out media.Project
	if err := json.Unmarshal(data, &out); err != nil {
		panic(fmt.Sprintf("clone fixture project: %v", err))
	}
	return &out
}
