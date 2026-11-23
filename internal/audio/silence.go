// Package audio inspects PCM audio blocks and reports merged silence runs.
// Silence output is descriptive only: callers never feed it back into the
// video segment cutter.
package audio

import (
	"encoding/binary"

	"medpipe/internal/model"
)

// Detector flags blocks whose peak absolute sample amplitude stays at or
// below Threshold for at least MinSamples samples in a row.
type Detector struct {
	Threshold  int
	MinSamples int
}

// NewDetector defaults to digital-zero detection with one block as the
// minimum run length.
func NewDetector(threshold, minSamples int) *Detector {
	if minSamples <= 0 {
		minSamples = 1
	}
	return &Detector{Threshold: threshold, MinSamples: minSamples}
}

// peakAmplitude scans little-endian signed 16-bit PCM, regardless of channel
// count; every interleaved sample counts toward the run length.
func peakAmplitude(payload []byte) (peak int, samples int) {
	samples = len(payload) / 2
	for i := 0; i+1 < len(payload); i += 2 {
		v := int(int16(binary.LittleEndian.Uint16(payload[i : i+2])))
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	return peak, samples
}

// blockSilent reports whether the block meets the configured silence rule.
func (d *Detector) blockSilent(b *model.AudioBlock) bool {
	peak, samples := peakAmplitude(b.Payload)
	if samples < d.MinSamples {
		return false
	}
	return peak <= d.Threshold
}

// Detect walks blocks in timeline order and merges adjacent silent blocks
// into single intervals. Each interval carries absolute start/end instants
// and the total PCM sample count it covers.
func (d *Detector) Detect(blocks []*model.AudioBlock) []model.SilenceInterval {
	var out []model.SilenceInterval
	var cur *model.SilenceInterval
	curSamples := 0

	flush := func(endNS int64) {
		if cur != nil && curSamples >= d.MinSamples {
			cur.EndNS = endNS
			cur.Samples = curSamples
			out = append(out, *cur)
		}
		cur = nil
		curSamples = 0
	}

	for _, b := range blocks {
		if d.blockSilent(b) {
			if cur == nil {
				cur = &model.SilenceInterval{StartNS: b.TimestampNS}
				curSamples = 0
			}
			_, samples := peakAmplitude(b.Payload)
			curSamples += samples
			cur.EndNS = b.TimestampNS + blockDurationNS(b)
			continue
		}
		flush(b.TimestampNS)
	}
	if cur != nil {
		flush(cur.EndNS)
	}
	return out
}

// blockDurationNS estimates the covered span from the block's own sample
// count and rate, so silence end times do not depend on the next block.
func blockDurationNS(b *model.AudioBlock) int64 {
	if b.SampleRate <= 0 || b.Samples <= 0 {
		return 0
	}
	return int64(b.Samples) * 1_000_000_000 / int64(b.SampleRate)
}
