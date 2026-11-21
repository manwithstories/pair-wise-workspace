package audio

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Interval is one merged silent range on a channel.
type Interval struct {
	Track   string
	Channel int
	StartMs float64
	EndMs   float64
}

// Report is the locatable result of silence detection on one payload.
type Report struct {
	Track        string
	Intervals    []Interval
	Issues       []string // empty track, truncated bytes, ...
	BoundaryHits []string // frames sitting on the threshold boundary
	SilentFrames int
	TotalFrames  int
}

// AllSilent reports whether every analysed frame was silent.
func (r Report) AllSilent() bool {
	return r.TotalFrames > 0 && r.SilentFrames == r.TotalFrames
}

// Detect measures per-channel RMS energy over fixed frames of the
// interleaved 16-bit little-endian PCM payload. Empty payloads,
// truncated trailing bytes and threshold-boundary frames all produce
// locatable report entries and never block later segments.
func Detect(track string, pcm []byte, channels, rate int, thresholdDBFS, frameMs, offsetMs float64) Report {
	rep := Report{Track: track}
	if channels <= 0 || rate <= 0 || frameMs <= 0 {
		rep.Issues = append(rep.Issues, fmt.Sprintf(
			"track %s: invalid parameters channels=%d rate=%d frameMs=%v", track, channels, rate, frameMs))
		return rep
	}
	if len(pcm) == 0 {
		rep.Issues = append(rep.Issues, fmt.Sprintf("track %s: empty audio payload", track))
		return rep
	}
	bytesPerFrame := 2 * channels
	if rem := len(pcm) % bytesPerFrame; rem != 0 {
		rep.Issues = append(rep.Issues, fmt.Sprintf(
			"track %s: %d trailing bytes do not form a complete sample; ignored", track, rem))
		pcm = pcm[:len(pcm)-rem]
	}
	samplesPerFrame := int(float64(rate) * frameMs / 1000.0)
	if samplesPerFrame <= 0 {
		samplesPerFrame = 1
	}
	totalSamples := len(pcm) / bytesPerFrame
	nFrames := (totalSamples + samplesPerFrame - 1) / samplesPerFrame

	// silent[ch][frame]
	silent := make([][]bool, channels)
	for ch := range silent {
		silent[ch] = make([]bool, nFrames)
	}
	rep.TotalFrames = nFrames
	for f := 0; f < nFrames; f++ {
		start := f * samplesPerFrame
		end := start + samplesPerFrame
		if end > totalSamples {
			end = totalSamples
		}
		frameStartMs := offsetMs + float64(start)*1000.0/float64(rate)
		for ch := 0; ch < channels; ch++ {
			var sum float64
			for i := start; i < end; i++ {
				v := int16(binary.LittleEndian.Uint16(pcm[i*bytesPerFrame+ch*2:]))
				sum += float64(v) * float64(v)
			}
			rms := math.Sqrt(sum / float64(end-start))
			db := -math.MaxFloat64
			if rms > 0 {
				db = 20 * math.Log10(rms/32768.0)
			}
			if db < thresholdDBFS {
				silent[ch][f] = true
				if ch == 0 {
					rep.SilentFrames++
				}
			}
			if math.Abs(db-thresholdDBFS) <= 0.5 {
				rep.BoundaryHits = append(rep.BoundaryHits, fmt.Sprintf(
					"track %s ch %d frame %d at %.1f ms: %.2f dBFS within 0.5 dB of threshold",
					track, ch, f, frameStartMs, db))
			}
		}
	}
	// Merge consecutive silent frames into intervals per channel.
	for ch := 0; ch < channels; ch++ {
		start := -1
		for f := 0; f <= nFrames; f++ {
			isSilent := f < nFrames && silent[ch][f]
			if isSilent && start < 0 {
				start = f
			}
			if !isSilent && start >= 0 {
				rep.Intervals = append(rep.Intervals, Interval{
					Track:   track,
					Channel: ch,
					StartMs: offsetMs + float64(start*samplesPerFrame)*1000.0/float64(rate),
					EndMs:   offsetMs + float64(min(f*samplesPerFrame, totalSamples))*1000.0/float64(rate),
				})
				start = -1
			}
		}
	}
	return rep
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
