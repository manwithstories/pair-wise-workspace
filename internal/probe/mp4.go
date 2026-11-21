// Package probe reads container metadata from self-generated MP4 and MXF
// samples and emits the shared, time-base-normalised model.
package probe

import (
	"fmt"
	"io"
	"os"

	"github.com/abema/go-mp4"

	"mediad/internal/clock"
	"mediad/internal/model"
)

// Limits bounds the metadata a probe will allocate for one file.
type Limits struct {
	// MaxSamples caps the number of samples per track.
	MaxSamples int
	// MaxBoxPayload caps the size of a box payload read into memory.
	MaxBoxPayload int
}

// DefaultLimits are the limits used by the CLI.
func DefaultLimits() Limits {
	return Limits{MaxSamples: 5_000_000, MaxBoxPayload: 4 << 20}
}

func MP4(path string, limits Limits) (model.Container, error) {
	if limits.MaxSamples <= 0 {
		limits.MaxSamples = DefaultLimits().MaxSamples
	}
	if limits.MaxBoxPayload <= 0 {
		limits.MaxBoxPayload = DefaultLimits().MaxBoxPayload
	}
	f, err := os.Open(path)
	if err != nil {
		return model.Container{}, model.Wrap(model.KindInvalidInput, "probe.mp4", "open", err)
	}
	defer f.Close()
	return MP4Reader(f, limits)
}

func MP4Reader(r io.ReadSeeker, limits Limits) (model.Container, error) {
	traks, err := mp4.ExtractBox(r, nil, mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeTrak()})
	if err != nil {
		return model.Container{}, model.Wrap(model.KindInvalidMedia, "probe.mp4", "read moov/trak", err)
	}
	if len(traks) == 0 {
		return model.Container{}, model.New(model.KindInvalidMedia, "probe.mp4", "no trak boxes found")
	}
	out := model.Container{Format: "mp4"}
	for _, bi := range traks {
		tr, err := probeTrak(r, bi, limits)
		if err != nil {
			return model.Container{}, err
		}
		out.Tracks = append(out.Tracks, tr)
	}
	return out, nil
}

func probeTrak(r io.ReadSeeker, trak *mp4.BoxInfo, limits Limits) (model.Track, error) {
	var tr model.Track
	tr.Source = "mp4"
	tr.FrameRate = clock.Zero()

	tkhd, err := firstPayload(r, trak, mp4.BoxPath{mp4.BoxTypeTkhd()})
	if err != nil {
		return tr, err
	}
	th, ok := tkhd.(*mp4.Tkhd)
	if !ok {
		return tr, model.New(model.KindInvalidMedia, "probe.mp4", "tkhd payload type mismatch")
	}
	tr.ID = th.TrackID
	tr.Codec = "unknown"

	mdhdBox, err := firstPayload(r, trak, mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMdhd()})
	if err != nil {
		return tr, err
	}
	md, ok := mdhdBox.(*mp4.Mdhd)
	if !ok || md.Timescale == 0 {
		return tr, model.New(model.KindInvalidMedia, "probe.mp4", "mdhd missing or has zero timescale").WithTrack(tr.ID)
	}
	tr.Timescale = md.Timescale

	hdr, err := firstPayload(r, trak, mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeHdlr()})
	if err != nil {
		return tr, err
	}
	if hd, ok := hdr.(*mp4.Hdlr); ok {
		tr.Kind = kindFromHandler(hd.HandlerType)
	}
	if tr.Kind == "" {
		tr.Kind = model.Data
	}

	// Edit list: media_time is the media time that maps to timeline zero.
	if es, err := payloads(r, trak, mp4.BoxPath{mp4.BoxTypeEdts(), mp4.BoxTypeElst()}); err != nil {
		return tr, err
	} else if len(es) > 0 {
		if el, ok := es[0].(*mp4.Elst); ok && len(el.Entries) > 0 {
			mt := el.Entries[0].MediaTimeV0
			if el.GetVersion() == 1 {
				v := el.Entries[0].MediaTimeV1
				if v < -1<<31 || v > 1<<31 {
					return tr, model.New(model.KindInvalidMedia, "probe.mp4",
						fmt.Sprintf("elst media_time %d out of range", v)).WithTrack(tr.ID)
				}
				mt = int32(v)
			}
			if mt != -1 {
				// The edit maps media_time to timeline zero, so sample
				// media times are shifted back by that amount. The edit
				// offset itself is retained for reporting.
				tr.EditOffset, err = clock.FromTicks(int64(mt), int64(md.Timescale))
				if err != nil {
					return tr, model.Wrap(model.KindInvalidMedia, "probe.mp4", "elst media_time", err).WithTrack(tr.ID)
				}
			}
		}
	}

	stsd, err := sampleEntry(r, trak, &tr, limits)
	if err != nil {
		return tr, err
	}

	var cropErr error
	if err := rotationAndCrop(r, trak, th, stsd, &tr, &cropErr); err != nil {
		return tr, err
	}
	if err := colorFromMP4(r, stsd, &tr); err != nil {
		return tr, err
	}

	stts, err := firstPayload(r, trak, mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStts()})
	if err != nil {
		return tr, err
	}
	sttsBox, ok := stts.(*mp4.Stts)
	if !ok {
		return tr, model.New(model.KindInvalidMedia, "probe.mp4", "missing stts").WithTrack(tr.ID)
	}

	ctts, err := payloads(r, trak, mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeCtts()})
	if err != nil {
		return tr, err
	}
	var cttsBox *mp4.Ctts
	if len(ctts) > 0 {
		cttsBox, _ = ctts[0].(*mp4.Ctts)
	}

	stsc, err := firstPayload(r, trak, mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsc()})
	if err != nil {
		return tr, err
	}
	stscBox, ok := stsc.(*mp4.Stsc)
	if !ok {
		return tr, model.New(model.KindInvalidMedia, "probe.mp4", "missing stsc").WithTrack(tr.ID)
	}

	stsz, err := firstPayload(r, trak, mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsz()})
	if err != nil {
		return tr, err
	}
	stszBox, ok := stsz.(*mp4.Stsz)
	if !ok {
		return tr, model.New(model.KindInvalidMedia, "probe.mp4", "missing stsz (stz2 is unsupported)").WithTrack(tr.ID)
	}
	if int(stszBox.SampleCount) > limits.MaxSamples {
		return tr, model.New(model.KindOverLimit, "probe.mp4",
			fmt.Sprintf("track has %d samples, limit %d", stszBox.SampleCount, limits.MaxSamples)).WithTrack(tr.ID)
	}

	offsets, err := chunkOffsets(r, trak, tr.ID)
	if err != nil {
		return tr, err
	}

	stss, err := firstPayload(r, trak, mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStss()})
	if err != nil {
		return tr, err
	}
	var sync map[uint32]bool
	if stss != nil {
		ss, ok := stss.(*mp4.Stss)
		if !ok {
			return tr, model.New(model.KindInvalidMedia, "probe.mp4", "stss payload type mismatch").WithTrack(tr.ID)
		}
		sync = make(map[uint32]bool, len(ss.SampleNumber))
		for _, n := range ss.SampleNumber {
			sync[n] = true
		}
	}

	samples, err := buildSamples(trackTable{
		timescale: int64(md.Timescale),
		stts:      sttsBox,
		ctts:      cttsBox,
		stsc:      stscBox,
		stsz:      stszBox,
		chunks:    offsets,
		sync:      sync,
		start:     clock.Zero(),
		limit:     limits.MaxSamples,
	}, tr.ID)
	if err != nil {
		return tr, err
	}
	tr.Samples = samples
	if n := len(samples); n > 0 {
		last, _ := samples[n-1].End()
		if tr.Duration, err = last.Sub(clock.Zero()); err != nil {
			return tr, model.Wrap(model.KindInvalidMedia, "probe.mp4", "duration", err).WithTrack(tr.ID)
		}
		if samples[0].Duration.Sign() > 0 {
			if tr.FrameRate, err = clock.FromInt(1).Div(samples[0].Duration); err != nil {
				tr.FrameRate = clock.Zero()
			}
		}
	}
	if cropErr != nil {
		span := "empty"
		if tr.Duration.Sign() > 0 {
			end, _ := tr.End()
			span = fmt.Sprintf("[%s, %s]", tr.Start.String(), end.String())
		}
		return tr, model.New(model.KindInvalidMedia, "probe.mp4",
			fmt.Sprintf("crop out of range on %dx%d frame over time range %s: %v",
				tr.CodedWidth, tr.CodedHeight, span, cropErr)).WithTrack(tr.ID)
	}
	if err := tr.Validate(); err != nil {
		return tr, err
	}
	return tr, nil
}
