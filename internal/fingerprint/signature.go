package fingerprint

import (
	"hash/fnv"
	"math"
)

// FrameSample is one sampled video frame's compact luminance/payload bytes.
type FrameSample struct {
	PTSSec float64 `json:"pts_sec"`
	Data   []byte  `json:"-"`
}

type Options struct {
	SampleRate       int     `json:"sample_rate"`
	AudioWindowMS    int     `json:"audio_window_ms"`
	FrameSampleCount int     `json:"frame_sample_count"`
	AudioThreshold   float64 `json:"audio_threshold"`
	FrameThreshold   float64 `json:"frame_threshold"`
}

func DefaultOptions() Options {
	return Options{SampleRate: 48_000, AudioWindowMS: 100, FrameSampleCount: 7, AudioThreshold: 0.9, FrameThreshold: 0.8}
}

func (o Options) normalize() Options {
	d := DefaultOptions()
	if o.SampleRate <= 0 {
		o.SampleRate = d.SampleRate
	}
	if o.AudioWindowMS <= 0 {
		o.AudioWindowMS = d.AudioWindowMS
	}
	if o.FrameSampleCount <= 0 {
		o.FrameSampleCount = d.FrameSampleCount
	}
	if o.AudioThreshold <= 0 {
		o.AudioThreshold = d.AudioThreshold
	}
	if o.FrameThreshold <= 0 {
		o.FrameThreshold = d.FrameThreshold
	}
	return o
}

// Signature contains independent quick features and the collision-resistant
// full review digest. A duplicate is never declared from one quick hash alone.
type Signature struct {
	ID            string   `json:"id"`
	AudioWindows  []uint64 `json:"-"`
	SampledFrames []uint64 `json:"-"`
	DurationSec   float64  `json:"duration_sec"`
	ReviewSHA256  string   `json:"review_sha256"`
	AudioScore    float64  `json:"-"`
	FrameScore    float64  `json:"-"`
}

type Match struct {
	A          string  `json:"a"`
	B          string  `json:"b"`
	AudioScore float64 `json:"audio_score"`
	FrameScore float64 `json:"frame_score"`
	Verified   bool    `json:"verified"`
}

// Reviewer performs the expensive candidate-level comparison. Production uses
// full source/artifact SHA-256; tests can inject collision scenarios.
type Reviewer func(a, b Signature) bool

func Build(id string, audio []float64, frames []FrameSample, reviewSHA string, durationSec float64, opt Options) Signature {
	opt = opt.normalize()
	return Signature{
		ID:            id,
		AudioWindows:  HashAudioWindows(audio, opt.SampleRate, opt.AudioWindowMS),
		SampledFrames: HashFrameSamples(frames, opt.FrameSampleCount),
		DurationSec:   durationSec,
		ReviewSHA256:  reviewSHA,
	}
}

func HashAudioWindows(samples []float64, sampleRate, windowMS int) []uint64 {
	if sampleRate <= 0 {
		sampleRate = 48_000
	}
	win := sampleRate * windowMS / 1000
	if win <= 0 {
		win = 1
	}
	var out []uint64
	for start := 0; start < len(samples); start += win {
		end := start + win
		if end > len(samples) {
			end = len(samples)
		}
		sum, sumSq := 0.0, 0.0
		for _, v := range samples[start:end] {
			sum += v
			sumSq += v * v
		}
		n := float64(end - start)
		rms := math.Sqrt(sumSq / n)
		mean := sum / n
		h := fnv.New64a()
		// Quantized RMS, DC offset and zero-crossing proxy make accidental
		// equality less likely than a single whole-clip hash.
		_, _ = h.Write([]byte{byte(math.Round(rms * 255)), byte(math.Round((mean + 1) * 64))})
		out = append(out, h.Sum64())
	}
	return out
}

func HashFrameSamples(frames []FrameSample, limit int) []uint64 {
	if limit <= 0 {
		limit = 7
	}
	step := 1
	if len(frames) > limit {
		step = len(frames) / limit
	}
	out := make([]uint64, 0, limit)
	for i := 0; i < len(frames) && len(out) < limit; i += step {
		h := fnv.New64a()
		_, _ = h.Write(frames[i].Data)
		out = append(out, h.Sum64())
	}
	return out
}

func AudioSimilarity(a, b Signature) float64 {
	return sequenceSimilarity(a.AudioWindows, b.AudioWindows)
}

func FrameSimilarity(a, b Signature) float64 {
	return sequenceSimilarity(a.SampledFrames, b.SampledFrames)
}

func sequenceSimilarity(a, b []uint64) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	// Compare against the shorter sequence; repeated broadcast packages often
	// have identical windows despite container padding differences.
	n := min(len(a), len(b))
	matches := 0
	for i := 0; i < n; i++ {
		if a[i] == b[i] {
			matches++
		}
	}
	return float64(matches) / float64(n)
}

// DetectDuplicates runs audio+frame screening and then review. Unverified hash
// collisions are returned as candidates but are not counted as duplicates.
func DetectDuplicates(sigs []Signature, opt Options, review Reviewer) ([]Match, int, error) {
	opt = opt.normalize()
	if review == nil {
		review = func(a, b Signature) bool {
			return a.ReviewSHA256 != "" && a.ReviewSHA256 == b.ReviewSHA256
		}
	}
	var matches []Match
	confirmed := 0
	for i := 0; i < len(sigs); i++ {
		for j := i + 1; j < len(sigs); j++ {
			as := AudioSimilarity(sigs[i], sigs[j])
			fs := FrameSimilarity(sigs[i], sigs[j])
			if as < opt.AudioThreshold || fs < opt.FrameThreshold {
				continue
			}
			m := Match{A: sigs[i].ID, B: sigs[j].ID, AudioScore: as, FrameScore: fs}
			m.Verified = review(sigs[i], sigs[j])
			if m.Verified {
				confirmed++
			}
			matches = append(matches, m)
		}
	}
	return matches, confirmed, nil
}
