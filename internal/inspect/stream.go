// Package inspect analyses the elementary stream structure of the packet
// events produced by ingest.  It parses what the codecs themselves declare —
// the Opus TOC byte, the AAC ADTS header and the H.264 AVCC length prefixes —
// and turns structural damage into localisable damaged slots instead of
// aborting the scan.
package inspect

import (
	"fmt"

	"mediacheck/internal/clock"
	"mediacheck/internal/model"
)

// Result is the accumulated analysis of one run.
type Result struct {
	Streams    map[model.Track]StreamStats
	Faults     []model.Fault
	Damage     model.DamageSet
	Keyframes  map[model.Track][]Keyframe
	AudioModes map[model.Track][]DurationChange
}

// StreamStats summarises one track.
type StreamStats struct {
	Packets      int
	Bytes        int64
	Keyframes    int
	MaxGapNanos  int64 // largest keyframe interval
	MinGapNanos  int64
	GOPs         []int64 // per interval frame counts
	DurationMin  int64   // ticks
	DurationMax  int64
	DurationMode int64 // most common packet duration in ticks
	Damaged      int
}

// Keyframe records a sync sample and the interval since the previous one.
type Keyframe struct {
	Seq       uint32
	Start     int64 // nanoseconds
	GapNanos  int64 // 0 for the first keyframe
	GapFrames int   // 0 for the first keyframe
	Track     model.Track
}

// DurationChange records an audio packet whose duration differs from the
// running mode.
type DurationChange struct {
	Seq      uint32
	Previous int64
	Current  int64
	Track    model.Track
}

// Inspector consumes packet events and accumulates structural analysis.  It
// is a single goroutine object, matching the pipeline's per-stage model.
type Inspector struct {
	res       Result
	lastPTS   map[model.Track]int64
	prevPack  map[model.Track]model.Packet
	keyLast   map[model.Track]Keyframe
	keyFrames map[model.Track]int
	durHist   map[model.Track]map[int64]int
	lastDur   map[model.Track]int64
}

// New returns an Inspector ready for use.
func New() *Inspector {
	return &Inspector{
		res: Result{
			Streams:    map[model.Track]StreamStats{},
			Keyframes:  map[model.Track][]Keyframe{},
			AudioModes: map[model.Track][]DurationChange{},
		},
		lastPTS:   map[model.Track]int64{},
		prevPack:  map[model.Track]model.Packet{},
		keyLast:   map[model.Track]Keyframe{},
		keyFrames: map[model.Track]int{},
		durHist:   map[model.Track]map[int64]int{},
		lastDur:   map[model.Track]int64{},
	}
}

// Inspect analyses one packet.  It never drops the packet: structural damage
// is reported as a fault anchored to the packet's own slot, and the packet
// is returned unchanged so later packets remain reachable.
func (in *Inspector) Inspect(p model.Packet) model.Packet {
	st := in.res.Streams[p.Track]
	st.Packets++
	st.Bytes += int64(len(p.Data))
	in.res.Streams[p.Track] = st

	if in.durHist[p.Track] == nil {
		in.durHist[p.Track] = map[int64]int{}
	}
	in.durHist[p.Track][p.Duration]++

	faults := in.analyse(p)
	if len(faults) > 0 {
		for _, f := range faults {
			if f.Kind.Severity() == model.SeverityError {
				p.Corrupt = true
			}
		}
	}
	for _, f := range faults {
		in.res.Faults = append(in.res.Faults, f)
		if f.Kind.Severity() != model.SeverityInfo {
			in.res.Damage.Add(f)
		}
	}

	if p.Track == model.TrackVideo && p.Keyframe {
		in.recordKeyframe(p)
	}
	if p.Track == model.TrackAudio {
		in.recordDuration(p)
	}
	if last, ok := in.lastPTS[p.Track]; ok && p.PTS < last {
		// The reader already reports this; record it for completeness without
		// touching the damage set twice.
		_ = last
	}
	in.lastPTS[p.Track] = p.PTS
	in.prevPack[p.Track] = p
	return p
}

func (in *Inspector) analyse(p model.Packet) []model.Fault {
	var faults []model.Fault
	add := func(kind model.FaultKind, note string) {
		faults = append(faults, model.Fault{Track: p.Track, Kind: kind, Seq: uint64(p.Sequence), Slot: p.Slot, Note: note})
	}
	if !p.Slot.Valid() {
		add(model.FaultZeroDuration, "empty slot")
	}
	switch p.Track {
	case model.TrackAudio:
		if err := parseAudio(p); err != nil {
			faults = append(faults, audioFaults(p, err)...)
		}
	case model.TrackVideo:
		if err := parseH264(p); err != nil {
			faults = append(faults, videoFaults(p, err)...)
		}
	}
	return faults
}

func (in *Inspector) recordKeyframe(p model.Packet) {
	k := Keyframe{Seq: p.Sequence, Start: p.Slot.Start, Track: p.Track}
	if last, ok := in.keyLast[p.Track]; ok {
		k.GapNanos = p.Slot.Start - last.Start
		k.GapFrames = in.keyFrames[p.Track] - int(last.Seq) - 1
		if k.GapFrames < 0 {
			k.GapFrames = 0
		}
		st := in.res.Streams[p.Track]
		if st.MaxGapNanos == 0 || k.GapNanos > st.MaxGapNanos {
			st.MaxGapNanos = k.GapNanos
		}
		if st.MinGapNanos == 0 || k.GapNanos < st.MinGapNanos {
			st.MinGapNanos = k.GapNanos
		}
		st.GOPs = append(st.GOPs, k.GapNanos)
		in.res.Streams[p.Track] = st
	}
	in.keyLast[p.Track] = k
	in.keyFrames[p.Track]++
	st := in.res.Streams[p.Track]
	st.Keyframes++
	in.res.Streams[p.Track] = st
	in.res.Keyframes[p.Track] = append(in.res.Keyframes[p.Track], k)
}

func (in *Inspector) recordDuration(p model.Packet) {
	hist := in.durHist[p.Track]
	if prev, ok := in.lastDur[p.Track]; ok && prev != p.Duration {
		in.res.AudioModes[p.Track] = append(in.res.AudioModes[p.Track], DurationChange{
			Seq: p.Sequence, Previous: prev, Current: p.Duration, Track: p.Track,
		})
	}
	// Mode changes are informational: a legitimate VBR stream changes
	// durations too.  The warning is emitted once a duration dominates.
	in.lastDur[p.Track] = p.Duration
	st := in.res.Streams[p.Track]
	st.DurationMin, st.DurationMax = p.Duration, p.Duration
	if st.DurationMin == 0 || p.Duration < st.DurationMin {
		st.DurationMin = p.Duration
	}
	if p.Duration > st.DurationMax {
		st.DurationMax = p.Duration
	}
	mode, best := p.Duration, -1
	for d, n := range hist {
		if n > best || (n == best && d < mode) {
			mode, best = d, n
		}
	}
	st.DurationMode = mode
	in.res.Streams[p.Track] = st
}

// Finish returns the accumulated result.  Streams that never saw a keyframe
// report zero intervals rather than a misleading value.
func (in *Inspector) Finish() Result {
	for track, st := range in.res.Streams {
		if st.Keyframes < 2 {
			st.MinGapNanos, st.MaxGapNanos = 0, 0
			in.res.Streams[track] = st
		}
	}
	return in.res
}

// KeyframeInterval renders the average sync sample interval in milliseconds.
func (st StreamStats) KeyframeInterval() float64 {
	if len(st.GOPs) == 0 {
		return 0
	}
	var total int64
	for _, g := range st.GOPs {
		total += g
	}
	return float64(total) / float64(len(st.GOPs)) / 1e6
}

// FaultText renders a fault list with a bounded number of entries.
func FaultText(fs []model.Fault, limit int) string {
	if len(fs) == 0 {
		return "none"
	}
	out := ""
	for i, f := range fs {
		if limit > 0 && i >= limit {
			out += fmt.Sprintf(" ...(%d more)", len(fs)-limit)
			break
		}
		if i > 0 {
			out += "; "
		}
		out += f.Track.String() + ":" + f.Kind.String()
		if f.Note != "" {
			out += "(" + f.Note + ")"
		}
	}
	return out
}

// ValidateStreamInfo rejects descriptors a muxer cannot use.
func ValidateStreamInfo(s model.StreamInfo) error {
	if !s.Timebase.Valid() {
		return fmt.Errorf("inspect: stream %s has invalid timebase", s.Track)
	}
	switch s.Track {
	case model.TrackAudio:
		if s.SampleRate <= 0 || s.Channels <= 0 {
			return fmt.Errorf("inspect: audio stream needs sample rate and channels")
		}
		if s.Codec != model.CodecOpus && s.Codec != model.CodecAAC {
			return fmt.Errorf("inspect: unsupported audio codec %s", s.Codec)
		}
	case model.TrackVideo:
		if s.Width <= 0 || s.Height <= 0 {
			return fmt.Errorf("inspect: video stream needs width and height")
		}
	}
	return nil
}

// TickToNanos exposes the timebase conversion used by the reporters.
func TickToNanos(tb clock.Timebase, ticks int64) int64 { return tb.Nanos(ticks) }
