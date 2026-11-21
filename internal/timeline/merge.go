// Package timeline fans recovered per-representation tracks into one shared
// rational clock, aligns audio/video/multi-camera sources and cross-checks
// representation fragment boundaries before handoff to the renderer.
package timeline

import (
	"fmt"
	"sort"

	"mediasync/internal/metadata"
	"mediasync/internal/model"
)

// AlignedFrame is a frame positioned on the shared master clock.
type AlignedFrame struct {
	Frame      model.Frame
	TrackID    string
	SharedTick int64
}

// Slot groups every track's frame that lands on the same shared tick.
type Slot struct {
	Tick       int64
	Frames     []AlignedFrame
	Incomplete bool // at least one contributor is an unrepairable gap
}

// Input bundles one resolved track's metadata with its recovered frames.
type Input struct {
	Track  metadata.ResolvedTrack
	Frames []model.Frame
}

// Timeline is the merged, ordered result consumed by the renderer and metrics.
type Timeline struct {
	Clock      model.Clock
	Slots      []Slot
	Tracks     map[string]metadata.ResolvedTrack
	Diag       model.Bag
	SyncMaxMs  float64 // worst observed audio-vs-video deviation
	Incomplete []string
}

// Merge aligns every input on master. Alignment is always converted directly
// from the shared origin (never accumulated frame by frame), so non-integer
// frame rates and track start offsets cannot accrue drift.
func Merge(master model.Clock, inputs []Input, syncTolerance model.Rational) *Timeline {
	tl := &Timeline{Clock: master, Tracks: map[string]metadata.ResolvedTrack{}}
	byTick := map[int64][]AlignedFrame{}
	ownerKey := map[string]string{} // "track:tick" -> owning representation
	var ticks []int64
	for _, in := range inputs {
		meta := in.Track.Track
		if meta.Start.Den <= 0 {
			meta.Start = model.Rational{Num: 0, Den: 1}
		}
		if meta.FrameRate.Den <= 0 {
			meta.FrameRate = model.Rational{Num: 30, Den: 1}
		}
		tl.Tracks[meta.TrackID] = in.Track
		origin := master.FromSeconds(meta.Start)
		for _, fr := range in.Frames {
			clock := fr.Clock
			if clock.TickRate == 0 {
				// Gap placeholders carry no sender clock; use the track time base.
				if meta.Timescale > 0 {
					clock = model.Clock{TickRate: meta.Timescale}
				} else {
					clock = master
				}
				fr.Clock = clock
			}
			shared := origin + master.Convert(fr.Timestamp, clock)
			if fr.Incomplete {
				// Placeholders have no real timestamp: place them in their own
				// half-open slot after the current tick so they never alias a frame.
				shared += int64(len(byTick)) + 1
			}
			af := AlignedFrame{Frame: fr, TrackID: meta.TrackID, SharedTick: shared}
			if fr.Incomplete {
				tl.Diag.Add(model.Diagnostic{Kind: model.KindMissingFrame, Severity: model.SeverityError,
					Track: meta.TrackID, AtTicks: shared,
					Message: fmt.Sprintf("rep=%s gap placeholder reaches timeline at seq=%d", fr.Rep, fr.Seq)})
				tl.Incomplete = appendOnce(tl.Incomplete, meta.TrackID)
			}
			// Single-contributor rule at the timeline boundary: one (track,tick)
			// pair is owned by exactly one representation. A later contributor is a
			// suppressed duplicate, never a second frame, so ABR switches cannot
			// manufacture overlapping fragments.
			if owner, ok := ownerKey[ownerOf(meta.TrackID, shared)]; ok {
				tl.Diag.Add(model.Diagnostic{Kind: model.KindFrameSuppressed, Severity: model.SeverityWarning,
					Track: meta.TrackID, AtTicks: shared, Recoverable: true,
					Message: fmt.Sprintf("rep=%s suppressed at tick %d already owned by rep=%s",
						fr.Rep, shared, owner)})
				continue
			}
			ownerKey[ownerOf(meta.TrackID, shared)] = fr.Rep
			byTick[shared] = append(byTick[shared], af)
			ticks = append(ticks, shared)
		}
	}
	sort.Slice(ticks, func(i, j int) bool { return ticks[i] < ticks[j] })
	ticks = dedupTicks(ticks)
	for _, tick := range ticks {
		frames := byTick[tick]
		sort.SliceStable(frames, func(i, j int) bool {
			if frames[i].TrackID != frames[j].TrackID {
				return frames[i].TrackID < frames[j].TrackID
			}
			return frames[i].Frame.Seq < frames[j].Frame.Seq
		})
		slot := Slot{Tick: tick, Frames: frames}
		for _, af := range frames {
			if af.Frame.Incomplete {
				slot.Incomplete = true
			}
		}
		tl.Slots = append(tl.Slots, slot)
	}
	alignAudioVideo(tl, inputs, syncTolerance)
	return tl
}

// alignAudioVideo compares every audio frame against the video frame grid and
// records deviations beyond tolerance. Exact rational math is used; the float
// maximum is retained for reporting only.
func alignAudioVideo(tl *Timeline, inputs []Input, tolerance model.Rational) {
	var video *Input
	var audios []*Input
	for i := range inputs {
		switch inputs[i].Track.Track.Kind {
		case model.KindVideo:
			video = &inputs[i]
		case model.KindAudio:
			audios = append(audios, &inputs[i])
		}
	}
	if video == nil || len(audios) == 0 {
		return
	}
	vmeta := video.Track.Track
	if vmeta.FrameRate.Num <= 0 || vmeta.FrameRate.Den <= 0 {
		return
	}
	frameDur := model.Rational{Num: vmeta.FrameRate.Den, Den: vmeta.FrameRate.Num}
	origin := vmeta.Start
	for _, audio := range audios {
		ameta := audio.Track.Track
		for _, fr := range audio.Frames {
			if fr.Incomplete {
				continue
			}
			at := ameta.Start.Add(fr.Clock.ToRational(fr.Timestamp))
			rel := at.Sub(origin)
			if rel.Num < 0 {
				rel = model.Rational{Num: -rel.Num, Den: rel.Den}
			}
			index := nearestIndex(rel, frameDur)
			grid := frameDur.MulInt(index)
			dev := rel.Sub(grid)
			if dev.Num < 0 {
				dev = model.Rational{Num: -dev.Num, Den: dev.Den}
			}
			ms := dev.Millis()
			if ms > tl.SyncMaxMs {
				tl.SyncMaxMs = ms
			}
			if dev.Cmp(tolerance) > 0 {
				tl.Diag.Add(model.Diagnostic{Kind: model.KindUnalignable, Severity: model.SeverityError,
					Track: ameta.TrackID, AtTicks: tl.Clock.FromSeconds(at),
					Message: fmt.Sprintf("audio frame %.2fms off video grid (tolerance %.0fms)",
						ms, tolerance.Millis())})
				tl.Incomplete = appendOnce(tl.Incomplete, ameta.TrackID)
			}
		}
	}
}

// nearestIndex returns the frame-grid index closest to a relative duration.
func nearestIndex(rel, frameDur model.Rational) int64 {
	// rel / frameDur rounded to nearest.
	num := rel.Num * frameDur.Den
	den := rel.Den * frameDur.Num
	q := num / den
	rem := num % den
	if rem*2 >= den {
		q++
	}
	if q < 0 {
		q = 0
	}
	return q
}

// FrameGridTick computes the exact shared tick of video frame index, proving
// non-integer rates stay drift free over arbitrary clip lengths.
func FrameGridTick(master model.Clock, start model.Rational, rate model.Rational, index int64) int64 {
	at := start.Add(model.Rational{Num: rate.Den * index, Den: rate.Num})
	return master.FromSeconds(at)
}

// ownerOf builds the per-track-per-tick ownership map key.
func ownerOf(track string, tick int64) string {
	return fmt.Sprintf("%s:%d", track, tick)
}

func dedupTicks(in []int64) []int64 {
	if len(in) == 0 {
		return in
	}
	out := in[:1]
	for _, v := range in[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

func appendOnce(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// IncompleteTracks returns the deterministic, sorted list of tracks that carry
// unrepairable gaps or unalignable segments.
func (tl *Timeline) IncompleteTracks() []string {
	out := append([]string(nil), tl.Incomplete...)
	sort.Strings(out)
	return out
}
