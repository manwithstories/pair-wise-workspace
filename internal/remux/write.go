// Package remux losslessly re-packages demuxed tracks onto the common
// 90 kHz integer timebase. Compressed sample payloads are copied
// verbatim; DTS/PTS wrap-around is corrected and tracks whose timestamps
// cannot be made monotonic are rejected. It never decodes or re-encodes.
package remux

import (
	"crypto/sha256"
	"fmt"
	"io"

	"archive/internal/clock"
	"archive/internal/errs"
	"archive/internal/mapping"
	"archive/internal/mp4"
)

// Options controls remuxing.
type Options struct {
	// OnFragment runs after each written fragment with the total output
	// size so far; the checkpoint layer uses it to persist progress.
	OnFragment func(written int64) error
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
		conv[m.SourceID] = &convState{srcTimescale: src.Timescale, clockBits: src.ClockBits}
		muxTracks[i] = mp4.MuxTrack{
			ID: uint32(i + 1), Type: m.Type, Codec: m.Codec,
			Timescale: clock.CommonTimescale, Language: m.Language,
			Width: 640, Height: 360,
		}
	}
	mux, err := mp4.NewMuxer(out, muxTracks)
	if err != nil {
		return nil, errs.New(errs.KindRemux, asset, "header", "cannot write fMP4 header", err)
	}
	res := &Result{Tracks: mapped}
	hash := sha256.New()
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
			samples, err := convertTrack(asset, loc, src, cs, hash)
			if err != nil {
				return nil, err
			}
			frags[i] = mp4.FragTrack{Samples: samples}
			res.Samples += len(samples)
		}
		if err := mux.AddFragment(frags); err != nil {
			return nil, errs.Newf(errs.KindRemux, asset, fmt.Sprintf("segment %d", segIdx), "cannot write fragment: %v", err)
		}
		if opts.OnFragment != nil {
			if err := opts.OnFragment(mux.Offset()); err != nil {
				return nil, err
			}
		}
	}
	res.BytesWritten = mux.Offset()
	copy(res.PayloadHash[:], hash.Sum(nil))
	return res, nil
}

// convertTrack unwraps and rescales one track's samples to the common
// timebase, hashing payloads in output order.
func convertTrack(asset, loc string, src mp4.Track, cs *convState, hash io.Writer) ([]mp4.Sample, error) {
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
		hash.Write(s.Data)
		out = append(out, mp4.Sample{
			DTS: dts, CTSOffset: int32(cts), Duration: uint32(dur),
			Keyframe: s.Keyframe, Data: s.Data,
		})
	}
	return out, nil
}
