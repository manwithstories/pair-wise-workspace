package media

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Kind distinguishes the elementary streams a delivery package carries.
type Kind int

const (
	KindVideo Kind = iota + 1
	KindAudio
)

// String renders the track kind.
func (k Kind) String() string {
	switch k {
	case KindVideo:
		return "video"
	case KindAudio:
		return "audio"
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

// codecFamily maps a CODECS string onto the family that must be paired safely
// during remux. HLS/DASH advertise e.g. "avc1.64001f,mp4a.40.2"; only families
// whose timebase and segment structure we can carry losslessly are accepted.
type codecFamily string

const (
	familyAVC  codecFamily = "avc"
	familyHEVC codecFamily = "hevc"
	familyAAC  codecFamily = "aac"
	familyAC3  codecFamily = "ac3"
	familyOpus codecFamily = "opus"
)

// supportedPairs lists the video/audio combinations aircheck can demux and
// remux without re-encoding. Anything else fails with FaultUnsupportedCodec.
var supportedPairs = map[codecFamily]map[codecFamily]bool{
	familyAVC:  {familyAAC: true, familyAC3: true},
	familyHEVC: {familyAAC: true},
}

var codecAliases = map[string]codecFamily{
	"avc1": familyAVC, "avc3": familyAVC, "h264": familyAVC,
	"hvc1": familyHEVC, "hev1": familyHEVC, "h265": familyHEVC,
	"mp4a": familyAAC, "aac": familyAAC,
	"ac-3": familyAC3, "ac3": familyAC3, "ec-3": familyAC3,
	"opus": familyOpus,
}

// TrackSpec is the immutable description of one elementary stream as recovered
// from a manifest. Offsets are absolute instants on the track's own timescale:
// a live window may start at a non-zero value, and a segment with
// EXT-X-DISCONTINUITY may jump.
type TrackSpec struct {
	Kind            Kind
	Codec           string  // CODECS attribute, e.g. "avc1.64001f"
	Timescale       int64   // ticks per second, from MP4 mdhd or TS-derived 90 kHz
	StartTick       int64   // first presentation instant on the track timescale
	DurationTk      int64   // total presented duration in track ticks
	Discontinuities []int64 // presentation instants, in track ticks, where the timeline jumps
	SampleRate      int     // audio only
	Channels        int     // audio only
	FrameRateNum    int64   // video only, e.g. 30/1
	FrameRateDen    int64
}

// family returns the codec family, or an error when the codec is unknown.
func (t TrackSpec) family() (codecFamily, error) {
	c := strings.TrimSpace(strings.ToLower(t.Codec))
	if i := strings.IndexByte(c, ','); i >= 0 {
		c = c[:i]
	}
	if i := strings.IndexByte(c, '.'); i >= 0 {
		c = c[:i]
	}
	if f, ok := codecAliases[c]; ok {
		return f, nil
	}
	return "", fmt.Errorf("%w: codec %q", ErrUnsupportedCodec, t.Codec)
}

// ErrUnsupportedCodec is returned for codec strings the remuxer cannot carry.
var ErrUnsupportedCodec = errors.New("media: unsupported codec")

// Validation failures.
var (
	ErrMissingTrack   = errors.New("media: package missing a video or audio track")
	ErrTrackTimescale = errors.New("media: invalid track timescale")
)

// Descriptor is a track after normalisation to the map reference clock.
type Descriptor struct {
	Kind       Kind
	Codec      string
	Family     codecFamily
	Start      Tick
	Duration   Duration
	Gaps       []Tick
	SampleRate int
	Channels   int
	FrameRate  float64
	// OffsetMs is how far this track's first instant sits from the package's
	// earliest first instant. Used for the align_max_ms acceptance metric.
	OffsetMs float64
}

// Timeline is the unified clock and track map for one package. All tracks
// share Ref; segment timestamps are converted into it exactly so that drift
// cannot accumulate across a 10 s window.
type Timeline struct {
	Ref    Clock
	Tracks []Descriptor
	// Start is the earliest first instant across tracks, in Ref.
	Start Tick
	// End is the latest last instant across tracks, in Ref.
	End Tick
}

// BuildTimeline validates the codec pairing and maps every track into a common
// rational clock. Offsets (non-zero start ticks) and disjoint track durations
// are preserved rather than silently shifted to zero.
func BuildTimeline(specs []TrackSpec) (*Timeline, error) {
	if len(specs) == 0 {
		return nil, ErrMissingTrack
	}
	have := map[Kind]bool{}
	for _, s := range specs {
		have[s.Kind] = true
	}
	if !have[KindVideo] || !have[KindAudio] {
		return nil, fmt.Errorf("%w: video=%v audio=%v", ErrMissingTrack, have[KindVideo], have[KindAudio])
	}

	// Validate compatibility of every video/audio pair present.
	var vfams, afams []codecFamily
	for _, s := range specs {
		f, err := s.family()
		if err != nil {
			return nil, err
		}
		if s.Timescale <= 0 {
			return nil, fmt.Errorf("%w: %s timescale %d", ErrTrackTimescale, s.Kind, s.Timescale)
		}
		switch s.Kind {
		case KindVideo:
			if s.FrameRateDen <= 0 || s.FrameRateNum <= 0 {
				return nil, fmt.Errorf("%w: video frame rate %d/%d", ErrUnsupportedCodec, s.FrameRateNum, s.FrameRateDen)
			}
			vfams = append(vfams, f)
		case KindAudio:
			if s.SampleRate <= 0 {
				return nil, fmt.Errorf("%w: audio sample rate %d", ErrUnsupportedCodec, s.SampleRate)
			}
			afams = append(afams, f)
		}
	}
	for _, v := range vfams {
		for _, a := range afams {
			if !supportedPairs[v][a] {
				return nil, fmt.Errorf("%w: %s+%s pair", ErrUnsupportedCodec, v, a)
			}
		}
	}

	refRate := int64(1)
	for _, s := range specs {
		r, err := LCM(refRate, s.Timescale)
		if err != nil {
			return nil, err
		}
		refRate = r
	}
	ref, err := NewClock(refRate)
	if err != nil {
		return nil, err
	}

	descs := make([]Descriptor, 0, len(specs))
	for _, s := range specs {
		f, _ := s.family()
		if s.DurationTk < 0 {
			return nil, fmt.Errorf("%w: negative duration", ErrTrackTimescale)
		}
		start, err := Tick{value: s.StartTick, clock: Clock{ticksPerSecond: s.Timescale}}.Convert(ref)
		if err != nil {
			return nil, err
		}
		dur, err := Duration{ticks: s.DurationTk, clock: Clock{ticksPerSecond: s.Timescale}}.Convert(ref)
		if err != nil {
			return nil, err
		}
		d := Descriptor{
			Kind: s.Kind, Codec: s.Codec, Family: f,
			Start: start, Duration: dur,
			SampleRate: s.SampleRate, Channels: s.Channels,
		}
		if s.Kind == KindVideo {
			d.FrameRate = float64(s.FrameRateNum) / float64(s.FrameRateDen)
		}
		for _, g := range s.Discontinuities {
			gap, err := Tick{value: g, clock: Clock{ticksPerSecond: s.Timescale}}.Convert(ref)
			if err != nil {
				return nil, fmt.Errorf("discontinuity: %w", err)
			}
			d.Gaps = append(d.Gaps, gap)
		}
		descs = append(descs, d)
	}

	sort.Slice(descs, func(i, j int) bool { return descs[i].Start.Value() < descs[j].Start.Value() })

	tl := &Timeline{Ref: ref, Tracks: descs}
	tl.Start = descs[0].Start
	firstEnd, err := descs[0].Start.Add(descs[0].Duration)
	if err != nil {
		return nil, err
	}
	tl.End = firstEnd
	for i := range descs {
		if descs[i].Start.Value() < tl.Start.Value() {
			tl.Start = descs[i].Start
		}
		end, err := descs[i].Start.Add(descs[i].Duration)
		if err != nil {
			return nil, err
		}
		if end.Value() > tl.End.Value() {
			tl.End = end
		}
	}
	for i := range descs {
		// OffsetMs is not drift: it is the intentional relative start that a
		// discontinuity or a padded leading frame legitimately introduces.
		off, err := descs[i].Start.Sub(tl.Start)
		if err != nil {
			return nil, err
		}
		descs[i].OffsetMs = off.Milliseconds()
	}
	return tl, nil
}

// Span returns the package duration from earliest start to latest end.
func (tl *Timeline) Span() Duration {
	if tl == nil {
		return Duration{}
	}
	d, err := tl.End.Sub(tl.Start)
	if err != nil {
		return Duration{}
	}
	return d
}

// AlignMaxMs reports the largest absolute first-instant offset between any two
// tracks. The demo requires < 10 ms; a discontinuity-free package with matching
// segment boundaries should be near zero.
func (tl *Timeline) AlignMaxMs() float64 {
	if len(tl.Tracks) < 2 {
		return 0
	}
	max := 0.0
	for i := range tl.Tracks {
		for j := i + 1; j < len(tl.Tracks); j++ {
			d := tl.Tracks[i].OffsetMs - tl.Tracks[j].OffsetMs
			if d < 0 {
				d = -d
			}
			if d > max {
				max = d
			}
		}
	}
	return max
}

// Track returns the first descriptor of the requested kind.
func (tl *Timeline) Track(k Kind) (Descriptor, bool) {
	for _, d := range tl.Tracks {
		if d.Kind == k {
			return d, true
		}
	}
	return Descriptor{}, false
}

// ParseFrameRate parses "30000/1001" or "30" into a rational.
func ParseFrameRate(s string) (num, den int64, err error) {
	if s == "" {
		return 0, 0, fmt.Errorf("%w: empty frame rate", ErrUnsupportedCodec)
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		num, err = strconv.ParseInt(s[:i], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("frame rate %q: %w", s, err)
		}
		den, err = strconv.ParseInt(s[i+1:], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("frame rate %q: %w", s, err)
		}
		if num <= 0 || den <= 0 {
			return 0, 0, fmt.Errorf("%w: frame rate %q", ErrUnsupportedCodec, s)
		}
		return num, den, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return 0, 0, fmt.Errorf("%w: frame rate %q", ErrUnsupportedCodec, s)
	}
	return int64(f * 1000), 1000, nil
}
