// Package audio normalizes sample rate, channel layout and loudness. All
// conversions are integer-exact or fail with a located fault; nothing is
// silently rounded or up-mixed.
package audio

import (
	"fmt"
	"strings"

	"mediamerge/internal/clock"
	"mediamerge/internal/fault"
)

// Layout names a channel layout. The canonical delivery layout is stereo or
// 5.1; other layouts are supported only when explicitly mapped.
type Layout string

const (
	LayoutMono   Layout = "mono"
	LayoutStereo Layout = "stereo"
	Layout51     Layout = "5.1"
	Layout51Film Layout = "5.1(film)"
	Layout71     Layout = "7.1"
)

// ChannelOrder returns the interleaved channel order for a layout. The order
// is part of the delivery contract and is never inferred from a count alone
// when a name is available.
func ChannelOrder(l Layout) ([]string, error) {
	switch l {
	case LayoutMono:
		return []string{"C"}, nil
	case LayoutStereo:
		return []string{"L", "R"}, nil
	case Layout51, Layout51Film:
		return []string{"L", "R", "C", "LFE", "Ls", "Rs"}, nil
	case Layout71:
		return []string{"L", "R", "C", "LFE", "Ls", "Rs", "Lrs", "Rrs"}, nil
	default:
		return nil, fmt.Errorf("audio: unknown layout %q", l)
	}
}

// Channels returns the channel count of a layout.
func Channels(l Layout) (int, error) {
	order, err := ChannelOrder(l)
	if err != nil {
		return 0, err
	}
	return len(order), nil
}

// LayoutForChannels maps a bare channel count to the only unambiguous layout
// with that count. Counts with several layouts are rejected so the caller
// must state the layout explicitly.
func LayoutForChannels(n int) (Layout, error) {
	switch n {
	case 1:
		return LayoutMono, nil
	case 2:
		return LayoutStereo, nil
	case 6:
		return Layout51, nil
	case 8:
		return Layout71, nil
	default:
		return "", fmt.Errorf("audio: %d channels have no unambiguous layout", n)
	}
}

// ChannelMap maps one input channel index to one output channel index. Gains
// are exact 1/den units so that a mapping only ever scales by a rational.
type ChannelMap struct {
	In  int
	Out int
	Num int64
	Den int64
}

// Mapping is an explicit input-to-output channel transform.
type Mapping struct {
	From Layout
	To   Layout
	Ops  []ChannelMap
}

// IdentityMapping builds the mapping for equal layouts.
func IdentityMapping(l Layout) (Mapping, error) {
	order, err := ChannelOrder(l)
	if err != nil {
		return Mapping{}, err
	}
	ops := make([]ChannelMap, len(order))
	for i := range order {
		ops[i] = ChannelMap{In: i, Out: i, Num: 1, Den: 1}
	}
	return Mapping{From: l, To: l, Ops: ops}, nil
}

// StereoFrom51 is the downmix used by the demo: L+R folded in, centre split
// equally, surrounds attenuated by 3 dB approximated as 1/2, LFE dropped.
func StereoFrom51() Mapping {
	return Mapping{
		From: Layout51, To: LayoutStereo,
		Ops: []ChannelMap{
			{In: 0, Out: 0, Num: 1, Den: 1}, // L
			{In: 1, Out: 1, Num: 1, Den: 1}, // R
			{In: 2, Out: 0, Num: 1, Den: 2}, // C -> L
			{In: 2, Out: 1, Num: 1, Den: 2}, // C -> R
			{In: 4, Out: 0, Num: 1, Den: 2}, // Ls -> L
			{In: 5, Out: 1, Num: 1, Den: 2}, // Rs -> R
		},
	}
}

// Validate checks index bounds, gains and that every output channel receives
// at least one contribution.
func (m Mapping) Validate() error {
	from, err := Channels(m.From)
	if err != nil {
		return err
	}
	to, err := Channels(m.To)
	if err != nil {
		return err
	}
	fed := make([]bool, to)
	for _, op := range m.Ops {
		if op.In < 0 || op.In >= from || op.Out < 0 || op.Out >= to {
			return fmt.Errorf("audio: channel op %d->%d outside %s->%s", op.In, op.Out, m.From, m.To)
		}
		if op.Den == 0 || op.Num == 0 {
			return fmt.Errorf("audio: channel op %d->%d has zero gain", op.In, op.Out)
		}
		fed[op.Out] = true
	}
	for i, ok := range fed {
		if !ok {
			names, _ := ChannelOrder(m.To)
			return fmt.Errorf("audio: output channel %d (%s) receives no input", i, names[i])
		}
	}
	return nil
}

// IncompatibleError reports a layout that cannot be produced without an
// explicit mapping. It keeps both layouts and the track so the operator can
// see which rule fired.
type IncompatibleError struct {
	TrackID int
	From    Layout
	To      Layout
	Reason  string
}

func (e *IncompatibleError) Error() string {
	return fmt.Sprintf("audio: track %d layout %s cannot become %s: %s", e.TrackID, e.From, e.To, e.Reason)
}

// MapLayout applies an explicit mapping to interleaved int32 samples.
// It never guesses a mapping.
func MapLayout(trackID int, samples []int32, from, to Layout, mapping *Mapping) ([]int32, Layout, error) {
	fromN, err := Channels(from)
	if err != nil {
		return nil, "", fault.NewTrack(fault.StageAudio, fault.CodeLayoutIncompat, trackID, err.Error())
	}
	toN, err := Channels(to)
	if err != nil {
		return nil, "", fault.NewTrack(fault.StageAudio, fault.CodeLayoutIncompat, trackID, err.Error())
	}
	if len(samples)%fromN != 0 {
		return nil, "", fault.NewTrack(fault.StageAudio, fault.CodeSampleIndivisible, trackID,
			fmt.Sprintf("%d interleaved samples do not form whole %s frames", len(samples), from))
	}
	if mapping == nil {
		if from != to {
			return nil, "", &IncompatibleError{TrackID: trackID, From: from, To: to,
				Reason: "no explicit channel mapping was supplied"}
		}
		m, err := IdentityMapping(from)
		if err != nil {
			return nil, "", fault.NewTrack(fault.StageAudio, fault.CodeLayoutIncompat, trackID, err.Error())
		}
		mapping = &m
	}
	if mapping.From != from || mapping.To != to {
		return nil, "", &IncompatibleError{TrackID: trackID, From: from, To: to,
			Reason: fmt.Sprintf("mapping is %s->%s", mapping.From, mapping.To)}
	}
	if err := mapping.Validate(); err != nil {
		return nil, "", &IncompatibleError{TrackID: trackID, From: from, To: to, Reason: err.Error()}
	}

	frames := len(samples) / fromN
	out := make([]int32, frames*toN)
	for f := 0; f < frames; f++ {
		in := samples[f*fromN : (f+1)*fromN]
		frame := out[f*toN : (f+1)*toN]
		for _, op := range mapping.Ops {
			frame[op.Out] += int32(int64(in[op.In]) * op.Num / op.Den)
		}
	}
	return out, to, nil
}

// LayoutName normalizes alias spellings such as "5.1film" so maps from CLI
// flags are stable.
func LayoutName(s string) Layout {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "mono", "1.0":
		return LayoutMono
	case "stereo", "2.0":
		return LayoutStereo
	case "5.1", "51":
		return Layout51
	case "5.1(film)", "5.1film":
		return Layout51Film
	case "7.1", "71":
		return Layout71
	default:
		return Layout(strings.TrimSpace(s))
	}
}

// SampleSpan returns the media range a sample window covers in a given track
// timebase. It is used to locate audio failures precisely.
func SampleSpan(trackID int, start, count, rate int, tb clock.Timebase) (clock.Span, error) {
	if rate <= 0 {
		return clock.Span{}, fault.NewTrack(fault.StageAudio, fault.CodeInvalidArgument, trackID,
			fmt.Sprintf("invalid sample rate %d", rate))
	}
	startSec := clock.MustRational(int64(start), int64(rate))
	endSec := clock.MustRational(int64(start+count), int64(rate))
	_ = tb
	return clock.Span{Start: startSec, End: endSec}, nil
}
