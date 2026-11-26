package timeline

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"pairflow/internal/media"
)

// RawTime is the JSON form of a rational time: seconds as num/den.
type RawTime struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

func (r RawTime) Media() (media.MediaTime, *media.Error) {
	return media.NewTime(r.Num, r.Den)
}

// AudioSpec describes the source audio of a clip exactly as delivered by a
// device; normalize.go maps it to the pipeline's 48 kHz / 6-channel layout.
type AudioSpec struct {
	Rate     int    `json:"rate_hz"`
	Channels int    `json:"channels"`
	Label    string `json:"label"`
}

// VideoSpec describes a clip's color description and generated pattern. There
// is no embedded bitmap data: fixtures are synthesized from these fields.
type VideoSpec struct {
	Space   string `json:"space"`   // srgb | rec709
	Pattern string `json:"pattern"` // bars | ramp | rings
	Tint    string `json:"tint"`    // teal | amber | neutral
}

// Clip is one edit on a track.
type Clip struct {
	ID    string     `json:"id"`
	Start RawTime    `json:"start"`
	End   RawTime    `json:"end"`
	Audio *AudioSpec `json:"audio,omitempty"`
	Video *VideoSpec `json:"video,omitempty"`
}

// Transition joins two clips on a track. Only cross dissolves are supported;
// any other kind is rejected rather than silently ignored.
type Transition struct {
	Kind     string  `json:"kind"`
	FromClip string  `json:"from"`
	ToClip   string  `json:"to"`
	Duration RawTime `json:"duration"`
}

// Track groups clips that share a medium.
type Track struct {
	ID          string       `json:"id"`
	Kind        string       `json:"kind"` // video | audio
	Clips       []Clip       `json:"clips"`
	Transitions []Transition `json:"transitions"`
}

// RawTimeline is the on-disk document produced by the generator.
type RawTimeline struct {
	Version     int     `json:"version"`
	FPS         int     `json:"fps"`
	SampleRate  int     `json:"sample_rate"`
	Duration    RawTime `json:"duration"`
	Seed        uint64  `json:"seed"`
	Description string  `json:"description"`
	Tracks      []Track `json:"tracks"`
}

const (
	supportedVersion    = 1
	supportedFPS        = 24
	supportedSampleRate = 48000
)

// DecodeBytes parses and structurally validates a timeline document. Unknown
// JSON fields, unknown track kinds and unknown transition kinds are errors, not
// silent fallbacks.
func DecodeBytes(data []byte, path string) (*RawTimeline, *media.Error) {
	var tl RawTimeline
	dec := json.NewDecoder(bytesReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&tl); err != nil {
		return nil, media.Wrap(err, media.ClassInput, "timeline.json_syntax", "invalid timeline document").WithPath(path)
	}
	if err := tl.validate(); err != nil {
		return nil, err.WithPath(path)
	}
	return &tl, nil
}

// DecodeFile reads a timeline document from path.
func DecodeFile(path string) (*RawTimeline, *media.Error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, media.Wrap(err, media.ClassInput, "timeline.unreadable", "cannot read timeline").WithPath(path)
	}
	return DecodeBytes(data, path)
}

func (tl *RawTimeline) validate() *media.Error {
	if tl.Version != supportedVersion {
		return media.Input("timeline.version", "unsupported version %d, want %d", tl.Version, supportedVersion)
	}
	if tl.FPS != supportedFPS {
		return media.Input("timeline.fps", "unsupported fps %d, want %d", tl.FPS, supportedFPS).WithField("fps")
	}
	if tl.SampleRate != supportedSampleRate {
		return media.Input("timeline.sample_rate", "unsupported sample rate %d, want %d", tl.SampleRate, supportedSampleRate).WithField("sample_rate")
	}
	if _, err := tl.Duration.Media(); err != nil {
		return err.WithField("duration")
	}
	if tl.Duration.Num == 0 {
		return media.Input("timeline.empty", "timeline duration must be positive").WithField("duration")
	}
	if len(tl.Tracks) == 0 {
		return media.Input("timeline.no_tracks", "timeline has no tracks").WithField("tracks")
	}
	seenTrack := map[string]bool{}
	for ti := range tl.Tracks {
		tr := &tl.Tracks[ti]
		if seenTrack[tr.ID] {
			return media.Input("timeline.duplicate_track", "duplicate track id %q", tr.ID).WithField(fmt.Sprintf("tracks[%d].id", ti))
		}
		seenTrack[tr.ID] = true
		switch tr.Kind {
		case "video", "audio":
		default:
			return media.Input("timeline.unknown_track_kind", "track %q has unknown kind %q", tr.ID, tr.Kind).
				WithField(fmt.Sprintf("tracks[%d].kind", ti))
		}
		if len(tr.Clips) == 0 {
			return media.Input("timeline.empty_track", "track %q has no clips", tr.ID)
		}
		seenClip := map[string]bool{}
		for ci := range tr.Clips {
			c := &tr.Clips[ci]
			f := func(name string) string {
				return fmt.Sprintf("tracks[%d].clips[%d].%s", ti, ci, name)
			}
			if c.ID == "" || seenClip[c.ID] {
				return media.Input("timeline.clip_id", "empty or duplicate clip id %q", c.ID).WithField(f("id"))
			}
			seenClip[c.ID] = true
			if _, err := c.Start.Media(); err != nil {
				return err.WithField(f("start"))
			}
			if _, err := c.End.Media(); err != nil {
				return err.WithField(f("end"))
			}
			switch tr.Kind {
			case "video":
				if c.Video == nil {
					return media.Input("timeline.clip_video", "video clip %q missing video spec", c.ID).WithField(f("video"))
				}
				if c.Audio != nil {
					return media.Input("timeline.clip_mixed", "video clip %q carries audio spec", c.ID).WithField(f("audio"))
				}
			case "audio":
				if c.Audio == nil {
					return media.Input("timeline.clip_audio", "audio clip %q missing audio spec", c.ID).WithField(f("audio"))
				}
				if c.Video != nil {
					return media.Input("timeline.clip_mixed", "audio clip %q carries video spec", c.ID).WithField(f("video"))
				}
			}
		}
		// Clips must appear in non-decreasing start order.
		for ci := 1; ci < len(tr.Clips); ci++ {
			a, _ := tr.Clips[ci-1].Start.Media()
			b, _ := tr.Clips[ci].Start.Media()
			if a.Cmp(b) > 0 {
				return media.Input("timeline.unsorted_clips", "track %q clips are not in start order at %q", tr.ID, tr.Clips[ci].ID).
					WithField(fmt.Sprintf("tracks[%d].clips[%d].start", ti, ci))
			}
		}
	}
	return nil
}

// bytesReader adapts a byte slice to the io.Reader the JSON decoder needs.
func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// SortTracks returns tracks ordered video-first then by id, for stable output.
func (tl *RawTimeline) SortTracks() []Track {
	out := make([]Track, len(tl.Tracks))
	copy(out, tl.Tracks)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Kind+":"+out[i].ID < out[j].Kind+":"+out[j].ID
	})
	return out
}
