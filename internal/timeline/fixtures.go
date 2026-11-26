package timeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"pairflow/internal/media"
)

// Generate builds a self-contained timeline fixture from a seed and duration.
// Nothing is read from disk: every clip, cut and dissolve is synthesized so
// reruns with the same seed are byte-identical.
//
// The result deliberately mixes source specifications so downstream stages
// must normalize rather than assume: topology cam A is rec.709, cam B is sRGB;
// audio covers 44.1/48/96 kHz and mono/stereo/5.1. Cut points and dissolve
// windows are chosen off the 24 fps frame grid so frame-boundary rounding is
// exercised.
func Generate(seed uint64, seconds int) (*RawTimeline, *media.Error) {
	if seconds <= 0 || seconds > 600 {
		return nil, media.Input("fixture.seconds", "seconds must be in (0,600], got %d", seconds)
	}
	// Seed only perturbs non-structural labels; the geometry stays stable so
	// metrics are reproducible for any seed.
	shade := []string{"teal", "amber", "neutral"}[seed%3]
	dur := RawTime{Num: int64(seconds), Den: 1}

	videoA := Track{
		ID:   "cam-a",
		Kind: "video",
		Clips: []Clip{
			{ID: "a1", Start: RawTime{0, 1}, End: RawTime{95, 100}, Video: &VideoSpec{Space: "rec709", Pattern: "bars", Tint: shade}},
			{ID: "a2", Start: RawTime{95, 100}, End: RawTime{19, 10}, Video: &VideoSpec{Space: "srgb", Pattern: "ramp", Tint: "amber"}},
			{ID: "a3", Start: RawTime{19, 10}, End: dur, Video: &VideoSpec{Space: "rec709", Pattern: "rings", Tint: "neutral"}},
		},
		Transitions: []Transition{
			{Kind: "cross_dissolve", FromClip: "a1", ToClip: "a2", Duration: RawTime{2, 5}}, // 0.40 s
			{Kind: "cross_dissolve", FromClip: "a2", ToClip: "a3", Duration: RawTime{1, 4}}, // 0.25 s
		},
	}
	videoB := Track{
		ID:   "cam-b",
		Kind: "video",
		Clips: []Clip{
			{ID: "b1", Start: RawTime{0, 1}, End: RawTime{6, 5}, Video: &VideoSpec{Space: "srgb", Pattern: "rings", Tint: "teal"}},
			{ID: "b2", Start: RawTime{6, 5}, End: dur, Video: &VideoSpec{Space: "rec709", Pattern: "bars", Tint: "amber"}},
		},
		Transitions: []Transition{
			{Kind: "cross_dissolve", FromClip: "b1", ToClip: "b2", Duration: RawTime{1, 2}}, // 0.50 s
		},
	}
	audioBoom := Track{
		ID:   "boom",
		Kind: "audio",
		Clips: []Clip{
			{ID: "boom1", Start: RawTime{0, 1}, End: RawTime{37, 25}, Audio: &AudioSpec{Rate: 44100, Channels: 1, Label: "field-mono"}},
			{ID: "boom2", Start: RawTime{37, 25}, End: dur, Audio: &AudioSpec{Rate: 48000, Channels: 2, Label: "studio-stereo"}},
		},
	}
	audioRemote := Track{
		ID:   "remote",
		Kind: "audio",
		Clips: []Clip{
			{ID: "rem1", Start: RawTime{0, 1}, End: RawTime{49, 50}, Audio: &AudioSpec{Rate: 96000, Channels: 6, Label: "remote-5.1"}},
			{ID: "rem2", Start: RawTime{49, 50}, End: dur, Audio: &AudioSpec{Rate: 48000, Channels: 1, Label: "remote-mono"}},
		},
	}
	return &RawTimeline{
		Version:     supportedVersion,
		FPS:         supportedFPS,
		SampleRate:  supportedSampleRate,
		Duration:    dur,
		Seed:        seed,
		Description: fmt.Sprintf("pairflow fixture seed=%d seconds=%d", seed, seconds),
		Tracks:      []Track{videoA, videoB, audioBoom, audioRemote},
	}, nil
}

// Marshal renders the timeline deterministically (stable key order via struct
// fields, two-space indent).
func (tl *RawTimeline) Marshal() ([]byte, *media.Error) {
	b, err := json.MarshalIndent(tl, "", "  ")
	if err != nil {
		return nil, media.Wrap(err, media.ClassInternal, "timeline.marshal", "cannot encode timeline")
	}
	return append(b, '\n'), nil
}

// Fingerprint is the stable timeline identity. It hashes the resolved edits —
// clip order, rational boundaries, dissolve windows and source descriptions —
// rather than the raw JSON, so it also pinpoints a semantic regression while
// staying identical for identical seeds.
func Fingerprint(res []Resolved) string {
	var b strings.Builder
	for _, tr := range res {
		fmt.Fprintf(&b, "T|%s|%s\n", tr.TrackID, tr.Kind)
		for _, s := range tr.Segments {
			fmt.Fprintf(&b, "S|%s|%s|%s|%s|%s|%s\n",
				s.ClipID, s.Start, s.End, s.DissolveIn, s.DissolveOut, specKey(s))
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// specKey renders the source specs of a segment compactly for hashing.
func specKey(s Segment) string {
	var parts []string
	if s.Audio != nil {
		parts = append(parts, fmt.Sprintf("a:%d/%d/%s", s.Audio.Rate, s.Audio.Channels, s.Audio.Label))
	}
	if s.Video != nil {
		parts = append(parts, fmt.Sprintf("v:%s/%s/%s", s.Video.Space, s.Video.Pattern, s.Video.Tint))
	}
	return strings.Join(parts, ";")
}

// TotalFrames is the shared video frame count for this timeline.
func (tl *RawTimeline) TotalFrames() (int, *media.Error) {
	d, err := tl.Duration.Media()
	if err != nil {
		return 0, err
	}
	n, err := d.FrameIndex()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// TotalSamples is the shared per-channel sample count for this timeline.
func (tl *RawTimeline) TotalSamples() (int, *media.Error) {
	d, err := tl.Duration.Media()
	if err != nil {
		return 0, err
	}
	n, err := d.SampleIndex()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}
