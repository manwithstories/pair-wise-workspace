// Package remux losslessly re-packages demuxed tracks onto the common
// 90 kHz integer timebase. Compressed sample payloads are copied
// verbatim; DTS/PTS wrap-around is corrected and tracks whose timestamps
// cannot be made monotonic are rejected. It never decodes or re-encodes.
package remux

import (
	"crypto/sha256"
	"fmt"
	"hash"
	"io"

	"archive/internal/clock"
	"archive/internal/errs"
	"archive/internal/mapping"
	"archive/internal/mp4"
)

// Options controls remuxing.
type Options struct {
	// SkipSegments marks a resume: the first SkipSegments entries of the
	// segments slice are already committed to the output. They are still
	// converted so unwrapping, monotonicity and track alignment stay
	// identical to a fresh run, but their fragments are not written.
	SkipSegments int
	// StartSeq is the fragment sequence number of the first written
	// fragment when resuming.
	StartSeq uint32
	// OnFragment runs after each written fragment with the absolute
	// segment index and the total output size so far.
	OnFragment func(seg int, written int64) error
}

// Result summarizes a remuxed asset.
type Result struct {
	Tracks       []mapping.MappedTrack
	BytesWritten int64
	Samples      int
	PayloadHash  [sha256.Size]byte // over all sample payloads, output order
}

type convState struct {
	srcTimescale uint32
	clockBits    int
	unwrap       clock.Unwrapper
	prev         int64 // last extended source DTS (64-bit clocks)
	init         bool
	shift        int64     // track alignment shift in common-timebase ticks
	hash         hash.Hash // per-track payload hash
}

func (cs *convState) extend(dts int64) (int64, error) {
	if cs.clockBits == 32 {
		return cs.unwrap.Unwrap(uint32(dts))
	}
	if cs.init && dts < cs.prev {
		return 0, errs.Newf(errs.KindClock, "", "remux",
			"non-monotonic DTS: %d after %d", dts, cs.prev)
	}
	cs.init, cs.prev = true, dts
	return dts, nil
}

// Remux converts segments (demuxed tracks per segment, in order) into one
// fMP4 asset written to out.
func Remux(asset string, segments [][]mp4.Track, out io.WriteSeeker, opts Options) (*Result, error) {
	if len(segments) == 0 {
		return nil, errs.New(errs.KindRemux, asset, "input", "no segments to remux", nil)
	}
	if opts.SkipSegments < 0 || opts.SkipSegments >= len(segments) {
		return nil, errs.Newf(errs.KindRemux, asset, "resume", "skip position %d outside 0..%d", opts.SkipSegments, len(segments)-1)
	}
	mapped := mapping.Map(segments[0])
	if len(mapped) == 0 {
		return nil, errs.New(errs.KindRemux, asset, "input", "no tracks to remux", nil)
	}
	byID := map[uint32]mp4.Track{}
	for _, t := range segments[0] {
		byID[t.ID] = t
	}
	conv := map[uint32]*convState{}
	muxTracks := make([]mp4.MuxTrack, len(mapped))
	for i, m := range mapped {
		src := byID[m.SourceID]
		if src.Timescale == 0 {
			return nil, errs.Newf(errs.KindClock, asset, fmt.Sprintf("track %d", m.SourceID), "zero timescale")
		}
		conv[m.SourceID] = &convState{srcTimescale: src.Timescale, clockBits: src.ClockBits, hash: sha256.New()}
		muxTracks[i] = mp4.MuxTrack{
			ID: uint32(i + 1), Type: m.Type, Codec: m.Codec,
			Timescale: clock.CommonTimescale, Language: m.Language,
			Width: 640, Height: 360,
		}
	}
	alignTracks(mapped, segments, conv)
	var mux *mp4.Muxer
	if opts.SkipSegments > 0 {
		mux = mp4.ResumeMuxer(out, muxTracks, opts.StartSeq)
	} else {
		var err error
		mux, err = mp4.NewMuxer(out, muxTracks)
		if err != nil {
			return nil, errs.New(errs.KindRemux, asset, "header", "cannot write fMP4 header", err)
		}
	}
	if opts.SkipSegments < 0 || opts.SkipSegments >= len(segments) {
		return nil, errs.Newf(errs.KindRemux, asset, "resume", "skip position %d outside 0..%d", opts.SkipSegments, len(segments)-1)
	}
	res := &Result{Tracks: mapped}
	for segIdx, segTracks := range segments {
		segByID := map[uint32]mp4.Track{}
		for _, t := range segTracks {
			segByID[t.ID] = t
		}
		frags := make([]mp4.FragTrack, len(mapped))
		for i, m := range mapped {
			src, ok := segByID[m.SourceID]
			if !ok {
				continue // track absent from this segment
			}
			cs := conv[m.SourceID]
			loc := fmt.Sprintf("segment %d track %d", segIdx, m.SourceID)
			samples, err := convertTrack(asset, loc, src, cs)
			if err != nil {
				return nil, err
			}
			frags[i] = mp4.FragTrack{Samples: samples}
			res.Samples += len(samples)
		}
		if segIdx < opts.SkipSegments {
			continue // committed by an earlier run; state kept in sync
		}
		if err := mux.AddFragment(frags); err != nil {
			return nil, errs.Newf(errs.KindRemux, asset, fmt.Sprintf("segment %d", segIdx), "cannot write fragment: %v", err)
		}
		if opts.OnFragment != nil {
			if err := opts.OnFragment(segIdx, mux.Offset()); err != nil {
				return nil, err
			}
		}
	}
	res.BytesWritten = mux.Offset()
	// combine per-track payload hashes in mapped (output track) order
	final := sha256.New()
	for _, m := range mapped {
		final.Write(conv[m.SourceID].hash.Sum(nil))
	}
	copy(res.PayloadHash[:], final.Sum(nil))
	return res, nil
}

// alignTracks computes a per-track shift on the common timebase so all
// tracks share one presentation origin. Independent 32-bit clocks wrap at
// different absolute instants, so after unwrap+rescale their epochs can
// differ by arbitrary amounts. Genuine small offsets (audio priming etc.)
// below alignThreshold are preserved; larger residuals are treated as
// clock-phase differences and aligned exactly to the earliest track.
func alignTracks(mapped []mapping.MappedTrack, segments [][]mp4.Track, conv map[uint32]*convState) {
	const alignThreshold = 5 * clock.CommonTimescale // 5 s in common ticks
	starts := map[uint32]int64{}
	cycles := map[uint32]int64{}
	var ref int64
	haveRef := false
	for _, m := range mapped {
		cs := conv[m.SourceID]
		for _, segTracks := range segments {
			t, ok := findTrack(segTracks, m.SourceID)
			if !ok || len(t.Samples) == 0 {
				continue
			}
			ext := t.Samples[0].DTS
			if cs.clockBits == 32 {
				var uw clock.Unwrapper
				ext, _ = uw.Unwrap(uint32(ext))
			}
			start, err := clock.Rescale(ext, cs.srcTimescale, clock.CommonTimescale)
			if err != nil {
				break
			}
			starts[m.SourceID] = start
			if cs.clockBits == 32 {
				if c, err := clock.Rescale(1<<32, cs.srcTimescale, clock.CommonTimescale); err == nil {
					cycles[m.SourceID] = c
				}
			}
			if !haveRef || start < ref {
				ref, haveRef = start, true
			}
			break
		}
	}
	for id, st := range starts {
		delta := ref - st
		if c := cycles[id]; c > 0 {
			// snap to whole 32-bit cycles, keeping the centered residual
			k := delta / c
			if rem := delta % c; rem > c/2 || (rem != 0 && delta < 0 && rem < -c/2) {
				if delta > 0 {
					k++
				} else {
					k--
				}
			}
			delta -= k * c
		}
		if delta < -alignThreshold || delta > alignThreshold {
			conv[id].shift = ref - st // arbitrary clock phase: align exactly
		} else {
			conv[id].shift = (ref - st) - delta // cycle snap only
		}
	}
	// normalize so the earliest track starts at zero on the asset timeline
	var minOut int64
	first := true
	for id, st := range starts {
		if out := st + conv[id].shift; first || out < minOut {
			minOut, first = out, false
		}
	}
	for id := range starts {
		conv[id].shift -= minOut
	}
}

func findTrack(tracks []mp4.Track, id uint32) (mp4.Track, bool) {
	for _, t := range tracks {
		if t.ID == id {
			return t, true
		}
	}
	return mp4.Track{}, false
}

// convertTrack unwraps and rescales one track's samples to the common
// timebase, hashing payloads into the track's conversion state.
func convertTrack(asset, loc string, src mp4.Track, cs *convState) ([]mp4.Sample, error) {
	out := make([]mp4.Sample, 0, len(src.Samples))
	for i, s := range src.Samples {
		ext, err := cs.extend(s.DTS)
		if err != nil {
			if e, ok := err.(*errs.Error); ok {
				e.Asset, e.Location = asset, loc
			}
			return nil, err
		}
		dts, err := clock.Rescale(ext, cs.srcTimescale, clock.CommonTimescale)
		if err != nil {
			return nil, err
		}
		dts += cs.shift
		dur, err := clock.Rescale(int64(s.Duration), cs.srcTimescale, clock.CommonTimescale)
		if err != nil {
			return nil, err
		}
		if dur <= 0 {
			return nil, errs.Newf(errs.KindClock, asset, loc, "sample %d duration collapses to zero on common timebase", i)
		}
		cts, err := clock.Rescale(int64(s.CTSOffset), cs.srcTimescale, clock.CommonTimescale)
		if err != nil {
			return nil, err
		}
		cs.hash.Write(s.Data)
		out = append(out, mp4.Sample{
			DTS: dts, CTSOffset: int32(cts), Duration: uint32(dur),
			Keyframe: s.Keyframe, Data: s.Data,
		})
	}
	return out, nil
}
