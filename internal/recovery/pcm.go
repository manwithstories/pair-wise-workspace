// Package recovery reconstructs missing PCM samples. Short consecutive loss is
// filled with configurable silence, larger bursts are rebuilt by neighborhood
// interpolation between the samples bracketing the gap, and anything outside
// the interpolation reach is marked unrecoverable rather than faked.
package recovery

import (
	"mediakernel/internal/clock"
	"mediakernel/internal/model"
)

// Policy configures the two recovery strategies.
type Policy struct {
	// Channels is the PCM channel count; every slot spans frameCount frames.
	Channels int
	// FramesPerPacket is the number of interleaved frames in one RTP slot.
	FramesPerPacket int
	// SilenceRunMax: missing runs up to this many packets are silence filled.
	SilenceRunMax int
	// InterpolateRunMax: longer missing runs up to this many packets are
	// linearly interpolated from bracketing real samples.
	InterpolateRunMax int
}

// DefaultPolicy returns conservative 48kHz stereo/20ms-packet defaults.
func DefaultPolicy() Policy {
	return Policy{
		Channels:          2,
		FramesPerPacket:   960,
		SilenceRunMax:     1,
		InterpolateRunMax: 5,
	}
}

// Slot is one sequence position on one audio stream.
type Slot struct {
	Seq     uint16
	Present bool
	Samples []int16 // interleaved, length FramesPerPacket*Channels when present
}

// Stats breaks sample provenance down for metrics.
type Stats struct {
	Real           int
	Recovered      int
	Unrecoverable  int
	SilenceFills   int
	Interpolations int
}

// Recoverer turns a sparse, possibly gappy slot sequence into one continuous
// AudioBlock with per-sample origin marks.
type Recoverer struct {
	policy Policy
	log    *model.Log
}

// New builds a Recoverer sharing the pipeline's fault log.
func New(policy Policy, log *model.Log) *Recoverer {
	if policy.Channels <= 0 || policy.FramesPerPacket <= 0 {
		policy = DefaultPolicy()
	}
	if policy.SilenceRunMax < 0 {
		policy.SilenceRunMax = 0
	}
	if policy.InterpolateRunMax < policy.SilenceRunMax {
		policy.InterpolateRunMax = policy.SilenceRunMax
	}
	return &Recoverer{policy: policy, log: log}
}

// Rebuild expects slots sorted by sequence (gaps represented by Present=false
// slots) and returns one block starting at start on the unified clock.
func (r *Recoverer) Rebuild(stream uint32, start clock.Ticks, slots []Slot) (*model.AudioBlock, Stats) {
	block := &model.AudioBlock{
		Stream:     stream,
		Start:      start,
		Channels:   r.policy.Channels,
		SampleRate: int(clock.AudioClockRate),
	}
	var stats Stats
	stride := r.policy.FramesPerPacket * r.policy.Channels
	for i := 0; i < len(slots); {
		if slots[i].Present {
			block.Samples = append(block.Samples, slots[i].Samples...)
			for range slots[i].Samples {
				block.Origins = append(block.Origins, model.OriginReal)
			}
			stats.Real += len(slots[i].Samples)
			i++
			continue
		}
		// Find the extent of this missing run.
		begin := i
		for i < len(slots) && !slots[i].Present {
			i++
		}
		run := i - begin
		missing := make([]int16, run*stride)
		origins := make([]model.Origin, run*stride)
		switch {
		case run <= r.policy.SilenceRunMax:
			r.fillSilence(missing, origins, slots[begin].Seq, &stats)
		case run <= r.policy.InterpolateRunMax && r.hasBrackets(slots, begin, i):
			r.fillInterpolated(missing, origins, slots, begin, i, stride, &stats)
		default:
			r.markUnrecoverable(origins, slots[begin].Seq, run, &stats)
		}
		block.Samples = append(block.Samples, missing...)
		block.Origins = append(block.Origins, origins...)
	}
	return block, stats
}

func (r *Recoverer) fillSilence(samples []int16, origins []model.Origin, seq uint16, st *Stats) {
	for i := range samples {
		samples[i] = 0
		origins[i] = model.OriginRecovered
	}
	st.Recovered += len(samples)
	st.SilenceFills++
	r.log.Add(model.MediaError{
		Category: model.CatLoss, Seq: seq,
		Detail:   "short consecutive loss filled with PCM silence",
		Decision: model.DecSilenceFill,
	})
}

func (r *Recoverer) hasBrackets(slots []Slot, begin, end int) bool {
	return begin > 0 && slots[begin-1].Present && end < len(slots) && slots[end].Present
}

func (r *Recoverer) fillInterpolated(samples []int16, origins []model.Origin, slots []Slot, begin, end, stride int, st *Stats) {
	left := slots[begin-1].Samples
	right := slots[end].Samples
	steps := (end-begin)*r.policy.FramesPerPacket + 1
	for f := 0; f < (end-begin)*r.policy.FramesPerPacket; f++ {
		pos := f + 1 // 1..steps-1 between the two boundary frames
		for c := 0; c < r.policy.Channels; c++ {
			lv := left[len(left)-stride+c]
			rv := right[c]
			v := int64(lv) + (int64(rv)-int64(lv))*int64(pos)/int64(steps)
			idx := f*r.policy.Channels + c
			samples[idx] = int16(v)
			origins[idx] = model.OriginRecovered
		}
	}
	st.Recovered += len(samples)
	st.Interpolations++
	r.log.Add(model.MediaError{
		Category: model.CatLoss, Seq: slots[begin].Seq,
		Detail:   "burst loss rebuilt by neighborhood interpolation",
		Decision: model.DecInterpolated,
	})
}

func (r *Recoverer) markUnrecoverable(origins []model.Origin, seq uint16, run int, st *Stats) {
	for i := range origins {
		origins[i] = model.OriginUnrecoverable
	}
	st.Unrecoverable += len(origins)
	r.log.Add(model.MediaError{
		Category: model.CatUnrecoverable, Seq: seq,
		Detail:   "loss run beyond interpolation reach or missing bracket",
		Decision: model.DecUnrecoverable,
	})
}
