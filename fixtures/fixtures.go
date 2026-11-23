// Package fixtures generates every input the demo and verification commands
// consume. Nothing here is checked-in binary data: all samples are synthesized
// deterministically from a seed so repeated runs are reproducible.
//
// Generated artifacts cover the required failure classes: valid multi-bitrate
// frame feeds, a byte-corrupted RTP input, a truncated subtitle input, a
// cyclic track graph and a configurable slow-consumer delay.
package fixtures

import (
	"math/rand"

	"mediad/diag"
	"mediad/segment"
	"mediad/timeline"
	"mediad/track"
)

// Spec describes one deterministic media generation request.
type Spec struct {
	Seed       int64
	DurationMS int64
	Bitrates   []int
	// KeyframeEvery is the frame interval between keyframes (90 at 30fps = 3s).
	KeyframeEvery int
	// PhaseJitter shifts each non-anchor representation's keyframe grid by up
	// to this many frames to exercise boundary extension (kept small).
	PhaseJitter int
}

// Feed is one generated representation plus its bitrate label.
type Feed struct {
	RepID  string
	Frames []segment.Frame
}

// GenerateFeeds builds one frame list per requested bitrate on a shared 30fps
// time base. The anchor (first bitrate) has the regular grid; later reps get
// deterministic, bounded keyframe phase jitter.
func GenerateFeeds(s Spec) ([]Feed, error) {
	if s.DurationMS <= 0 || len(s.Bitrates) == 0 {
		return nil, diag.New(diag.EParam, "", "fixture spec needs positive duration and bitrates")
	}
	rng := rand.New(rand.NewSource(s.Seed))
	frameCount := int(s.DurationMS / 1000 * 30)
	out := make([]Feed, 0, len(s.Bitrates))
	for i, br := range s.Bitrates {
		phase := 0
		if i > 0 && s.PhaseJitter > 0 {
			phase = rng.Intn(s.PhaseJitter + 1)
		}
		frames := make([]segment.Frame, 0, frameCount)
		for f := 0; f < frameCount; f++ {
			tt, err := timeline.FrameToTime(timeline.FR30, int64(f))
			if err != nil {
				return nil, err
			}
			key := (f+phase)%s.KeyframeEvery == 0
			frames = append(frames, segment.Frame{Time: tt, Keyframe: key})
		}
		out = append(out, Feed{RepID: bitrateLabel(br), Frames: frames})
	}
	return out, nil
}

func bitrateLabel(br int) string {
	return "v-" + itoa(br)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// ValidGraph builds the normal dependency graph: a timecode track feeds every
// representation, with a subtitle track alongside.
func ValidGraph(repIDs []string) (*track.Graph, error) {
	tr := []track.Track{
		{ID: "tc", Kind: track.KindTimecode},
		{ID: "sub-en", Kind: track.KindSRT, Deps: []string{"tc"}},
	}
	for _, id := range repIDs {
		tr = append(tr, track.Track{ID: id, Kind: track.KindRTP, Deps: []string{"tc"}})
	}
	return track.NewGraph(tr)
}

// CyclicGraph builds an intentionally cyclic graph for startup rejection demos.
func CyclicGraph() (*track.Graph, error) {
	return track.NewGraph([]track.Track{
		{ID: "a", Kind: track.KindRTP, Deps: []string{"c"}},
		{ID: "b", Kind: track.KindRTP, Deps: []string{"a"}},
		{ID: "c", Kind: track.KindRTP, Deps: []string{"b"}},
	})
}

// SRTInput generates a deterministic SRT byte stream with the requested number
// of cues, each three seconds long and spaced five seconds apart.
func SRTInput(cues int) []byte {
	var out []byte
	for i := 1; i <= cues; i++ {
		start := (i - 1) * 5000
		end := start + 3000
		out = append(out, srtBlock(i, start, end)...)
	}
	return out
}

func srtBlock(idx, startMS, endMS int) []byte {
	return []byte(
		itoa(idx) + "\n" +
			hhmmss(startMS) + " --> " + hhmmss(endMS) + "\n" +
			"cue " + itoa(idx) + " text\n\n")
}

func hhmmss(ms int) string {
	h := ms / 3600000
	ms %= 3600000
	m := ms / 60000
	ms %= 60000
	s := ms / 1000
	milli := ms % 1000
	return pad(h) + ":" + pad(m) + ":" + pad(s) + "," + pad3(milli)
}

func pad(n int) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}

func pad3(n int) string {
	switch {
	case n < 10:
		return "00" + itoa(n)
	case n < 100:
		return "0" + itoa(n)
	default:
		return itoa(n)
	}
}

// CorruptAt flips one byte at offset inside data (or the last byte when offset
// is beyond the slice), simulating located input corruption.
func CorruptAt(data []byte, offset int) []byte {
	out := append([]byte(nil), data...)
	if offset >= len(out) {
		offset = len(out) - 1
	}
	if offset < 0 {
		offset = 0
	}
	out[offset] ^= 0xFF
	return out
}

// TruncateAt cuts data to offset bytes, simulating a truncated input stream.
func TruncateAt(data []byte, offset int) []byte {
	if offset < 0 {
		offset = 0
	}
	if offset > len(data) {
		offset = len(data)
	}
	return append([]byte(nil), data[:offset]...)
}
