package analysis

import (
	"math"

	"github.com/asticode/go-astiav"
)

// Fingerprint is a content-derived signature. It is independent of container
// bytes and codec settings, so a re-encoded copy of the same content produces a
// near-identical fingerprint while genuinely different content does not.
type Fingerprint struct {
	Path string `json:"path"`
	// VideoHash is a 64-bit perceptual mean hash of a downscaled frame grid.
	VideoHash uint64 `json:"video_hash"`
	// VideoBits is the number of valid bits in VideoHash.
	VideoBits int `json:"video_bits"`
	// AudioEnvelope is a short, normalised loudness profile across the segment.
	AudioEnvelope []float32 `json:"audio_envelope"`
	// FrameCount anchors the fingerprint to a temporal length.
	FrameCount int `json:"frame_count"`
}

// fpGrid is the perceptual hash grid dimension (8x8 -> 64 bits).
const fpGrid = 8

// envelopeBins is the fixed-length audio loudness profile length.
const envelopeBins = 16

// FingerprintSegment decodes a segment and derives its content fingerprint.
func FingerprintSegment(path string) (*Fingerprint, error) {
	_, streams, closer, err := openSegment(path)
	if err != nil {
		return nil, err
	}
	defer closer()

	fp := &Fingerprint{Path: path}

	// Video: average a coarse luma grid across all frames for temporal stability.
	if _, hasVideo := streams[astiav.MediaTypeVideo]; hasVideo {
		var grid [fpGrid][fpGrid]float64
		var frames int
		err = decodeFrames(streams, astiav.MediaTypeVideo, func(f *astiav.Frame) error {
			g, err := lumaGrid(f)
			if err != nil {
				return err
			}
			for y := 0; y < fpGrid; y++ {
				for x := 0; x < fpGrid; x++ {
					grid[y][x] += g[y][x]
				}
			}
			frames++
			return nil
		})
		if err != nil {
			return nil, err
		}
		if frames > 0 {
			fp.FrameCount = frames
			fp.VideoHash = meanHash(grid, frames)
			fp.VideoBits = fpGrid * fpGrid
		}
	}

	// Audio: fixed-bin normalised loudness envelope.
	if _, hasAudio := streams[astiav.MediaTypeAudio]; hasAudio {
		envelope, err := audioEnvelope(streams)
		if err != nil {
			return nil, err
		}
		fp.AudioEnvelope = envelope
	}
	return fp, nil
}

// lumaGrid reduces a frame to an 8x8 luma matrix by block averaging. It works
// on a decoded Y plane copied out through FrameData.Bytes.
func lumaGrid(f *astiav.Frame) ([fpGrid][fpGrid]float64, error) {
	var g [fpGrid][fpGrid]float64
	w, h := f.Width(), f.Height()
	if w <= 0 || h <= 0 {
		return g, nil
	}
	buf, err := f.Data().Bytes(1)
	if err != nil {
		return g, err
	}
	// Bytes returns a tightly packed copy, so the Y-plane row stride is the
	// frame width rather than the aligned in-frame linesize.
	linesize := w
	for gy := 0; gy < fpGrid; gy++ {
		for gx := 0; gx < fpGrid; gx++ {
			x0 := gx * w / fpGrid
			x1 := (gx + 1) * w / fpGrid
			y0 := gy * h / fpGrid
			y1 := (gy + 1) * h / fpGrid
			var sum float64
			var n float64
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					idx := y*linesize + x
					if idx < len(buf) {
						sum += float64(buf[idx])
						n++
					}
				}
			}
			if n > 0 {
				g[gy][gx] = sum / n
			}
		}
	}
	return g, nil
}

// meanHash converts an averaged luma grid into a 64-bit perceptual hash: a bit
// is set when the cell is brighter than the grid mean. Small re-encode shifts
// barely change these comparisons, giving robustness to transcode drift.
func meanHash(grid [fpGrid][fpGrid]float64, frames int) uint64 {
	var total float64
	for y := 0; y < fpGrid; y++ {
		for x := 0; x < fpGrid; x++ {
			total += grid[y][x]
		}
	}
	mean := total / float64(fpGrid*fpGrid)
	var hash uint64
	bit := 0
	for y := 0; y < fpGrid; y++ {
		for x := 0; x < fpGrid; x++ {
			if grid[y][x] >= mean {
				hash |= 1 << bit
			}
			bit++
		}
	}
	return hash
}

// audioEnvelope builds a fixed-length normalised RMS profile so two copies of
// the same audio align regardless of absolute encoding level.
func audioEnvelope(streams map[astiav.MediaType]*decodedStream) ([]float32, error) {
	var frameRMS []float32
	err := decodeFrames(streams, astiav.MediaTypeAudio, func(f *astiav.Frame) error {
		ch := f.ChannelLayout().Channels()
		rms, err := frameRMSPerChannel(f, ch)
		if err != nil {
			return err
		}
		var sum float64
		for _, v := range rms {
			sum += float64(v) * float64(v)
		}
		frameRMS = append(frameRMS, float32(math.Sqrt(sum/float64(max(ch, 1)))))
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]float32, envelopeBins)
	if len(frameRMS) == 0 {
		return out, nil
	}
	binSize := float64(len(frameRMS)) / envelopeBins
	for b := 0; b < envelopeBins; b++ {
		lo := int(math.Floor(float64(b) * binSize))
		hi := int(math.Floor(float64(b+1) * binSize))
		if hi <= lo {
			hi = lo + 1
		}
		if hi > len(frameRMS) {
			hi = len(frameRMS)
		}
		var sum float64
		n := 0
		for k := lo; k < hi; k++ {
			sum += float64(frameRMS[k])
			n++
		}
		if n > 0 {
			out[b] = float32(sum / float64(n))
		}
	}
	normalizeEnvelope(out)
	return out, nil
}

func normalizeEnvelope(e []float32) {
	var peak float32
	for _, v := range e {
		if v > peak {
			peak = v
		}
	}
	if peak <= 0 {
		return
	}
	for i := range e {
		e[i] = e[i] / peak
	}
}

// hammingDistance counts differing bits between two 64-bit hashes.
func hammingDistance(a, b uint64) int {
	x := a ^ b
	n := 0
	for x != 0 {
		n++
		x &= x - 1
	}
	return n
}

// envelopeDistance returns the mean absolute difference between envelopes.
func envelopeDistance(a, b []float32) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n == 0 {
		return 1
	}
	var sum float64
	for i := 0; i < n; i++ {
		d := float64(a[i] - b[i])
		sum += math.Abs(d)
	}
	return sum / float64(n)
}

// Similarity reports whether two fingerprints describe the same content. The
// video hamming threshold tolerates transcode drift; the audio envelope must
// also agree. Different clips fail at least one test and are never merged.
func Similarity(a, b *Fingerprint) bool {
	if a == nil || b == nil {
		return false
	}
	videoOK := false
	if a.VideoBits > 0 && b.VideoBits > 0 {
		dist := hammingDistance(a.VideoHash, b.VideoHash)
		// Up to 10 of 64 bits may differ (re-encode / minor resize drift).
		videoOK = dist <= 10
	}
	audioOK := false
	if len(a.AudioEnvelope) > 0 && len(b.AudioEnvelope) > 0 {
		audioOK = envelopeDistance(a.AudioEnvelope, b.AudioEnvelope) <= 0.15
	}
	if videoOK && audioOK {
		return true
	}
	// Audio-only or video-only segments rely on whichever signal exists.
	if a.VideoBits == 0 && b.VideoBits == 0 {
		return audioOK
	}
	if len(a.AudioEnvelope) == 0 && len(b.AudioEnvelope) == 0 {
		return videoOK
	}
	return false
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
