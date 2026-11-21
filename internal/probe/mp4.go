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

// MP4 probes an ISO base media file.
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

// MP4Reader probes an MP4 from a seekable reader.
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

	if err := rotationAndCrop(r, trak, th, stsd, &tr); err != nil {
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
	if err := tr.Validate(); err != nil {
		return tr, err
	}
	return tr, nil
}

func kindFromHandler(h [4]byte) model.TrackKind {
	switch string(h[:]) {
	case "vide":
		return model.Video
	case "soun":
		return model.Audio
	default:
		return model.Data
	}
}

// sampleEntry locates the first sample description, records its codec and
// dimensions, and returns the stsd box so child boxes can be extracted.
func sampleEntry(r io.ReadSeeker, trak *mp4.BoxInfo, tr *model.Track, limits Limits) (*mp4.BoxInfo, error) {
	stds, err := mp4.ExtractBox(r, trak, mp4.BoxPath{
		mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsd(),
	})
	if err != nil {
		return nil, model.Wrap(model.KindInvalidMedia, "probe.mp4", "extract stsd", err).WithTrack(tr.ID)
	}
	if len(stds) == 0 {
		return nil, model.New(model.KindInvalidMedia, "probe.mp4", "missing stsd").WithTrack(tr.ID)
	}
	var entry *mp4.BoxInfo
	_, err = mp4.ReadBoxStructureFromInternal(r, stds[0], func(h *mp4.ReadHandle) (interface{}, error) {
		if entry == nil && len(h.Path) == 2 {
			cp := h.BoxInfo
			entry = &cp
		}
		return nil, nil
	})
	if err != nil {
		return nil, model.Wrap(model.KindInvalidMedia, "probe.mp4", "walk stsd", err).WithTrack(tr.ID)
	}
	if entry == nil {
		return nil, model.New(model.KindInvalidMedia, "probe.mp4", "stsd has no sample entry").WithTrack(tr.ID)
	}
	tr.Codec = entry.Type.String()

	if _, err := entry.SeekToPayload(r); err != nil {
		return nil, model.Wrap(model.KindInvalidMedia, "probe.mp4", "seek sample entry", err).WithTrack(tr.ID)
	}
	box, _, err := mp4.UnmarshalAny(r, entry.Type, entry.Size-entry.HeaderSize, entry.Context)
	if err != nil {
		return nil, model.Wrap(model.KindInvalidMedia, "probe.mp4", "parse sample entry", err).WithTrack(tr.ID)
	}
	switch v := box.(type) {
	case *mp4.VisualSampleEntry:
		tr.CodedWidth = uint32(v.Width)
		tr.CodedHeight = uint32(v.Height)
		tr.SampleAspect = clock.FromInt(1)
	case *mp4.AudioSampleEntry:
		tr.SampleAspect = clock.FromInt(1)
	}
	return stds[0], nil
}

func rotationAndCrop(r io.ReadSeeker, trak *mp4.BoxInfo, th *mp4.Tkhd, stsd *mp4.BoxInfo, tr *model.Track) error {
	rot, err := rotationFromMatrix(th.Matrix)
	if err != nil {
		return model.Wrap(model.KindInvalidMedia, "probe.mp4", "rotation", err).WithTrack(tr.ID)
	}
	tr.Rotation = rot
	tr.Crop = model.Crop{}

	// clap is not modelled by go-mp4 v1.4.1; read the raw payload.
	claps, err := mp4.ExtractBox(r, stsd, mp4.BoxPath{mp4.BoxTypeAny(), mp4.BoxTypeAny(), mp4.StrToBoxType("clap")})
	if err != nil {
		return model.Wrap(model.KindInvalidMedia, "probe.mp4", "extract clap", err).WithTrack(tr.ID)
	}
	if len(claps) > 0 {
		if _, err := claps[0].SeekToPayload(r); err != nil {
			return model.Wrap(model.KindInvalidMedia, "probe.mp4", "seek clap", err).WithTrack(tr.ID)
		}
		payload, err := readLimited(r, claps[0].Size-claps[0].HeaderSize, DefaultLimits().MaxBoxPayload)
		if err != nil {
			return model.Wrap(model.KindInvalidMedia, "probe.mp4", "read clap", err).WithTrack(tr.ID)
		}
		clap, err := parseClap(payload)
		if err != nil {
			return model.Wrap(model.KindInvalidMedia, "probe.mp4", "parse clap", err).WithTrack(tr.ID)
		}
		if tr.CodedWidth == 0 || tr.CodedHeight == 0 {
			return model.New(model.KindInvalidMedia, "probe.mp4", "clap on track without visual dimensions").WithTrack(tr.ID)
		}
		tr.Crop, err = clap.crop(tr.CodedWidth, tr.CodedHeight)
		if err != nil {
			return model.Wrap(model.KindInvalidMedia, "probe.mp4", "clap crop", err).WithTrack(tr.ID)
		}
	}

	pasps, err := mp4.ExtractBoxWithPayload(r, stsd, mp4.BoxPath{mp4.BoxTypeAny(), mp4.BoxTypeAny(), mp4.BoxTypePasp()})
	if err != nil {
		return model.Wrap(model.KindInvalidMedia, "probe.mp4", "extract pasp", err).WithTrack(tr.ID)
	}
	if len(pasps) > 0 {
		if p, ok := pasps[0].Payload.(*mp4.PixelAspectRatioBox); ok && p.HSpacing > 0 && p.VSpacing > 0 {
			if tr.SampleAspect, err = clock.New(int64(p.HSpacing), int64(p.VSpacing)); err != nil {
				return model.Wrap(model.KindInvalidMedia, "probe.mp4", "pasp", err).WithTrack(tr.ID)
			}
		}
	}
	return nil
}

func colorFromMP4(r io.ReadSeeker, stsd *mp4.BoxInfo, tr *model.Track) error {
	colrs, err := mp4.ExtractBoxWithPayload(r, stsd, mp4.BoxPath{mp4.BoxTypeAny(), mp4.BoxTypeAny(), mp4.BoxTypeColr()})
	if err != nil {
		return model.Wrap(model.KindInvalidMedia, "probe.mp4", "extract colr", err).WithTrack(tr.ID)
	}
	cands := tr.ColorCandidates
	for _, c := range colrs {
		box, ok := c.Payload.(*mp4.Colr)
		if !ok {
			continue
		}
		typ := string(box.ColourType[:])
		switch typ {
		case "nclx":
			cands = append(cands, model.ColorCandidate{
				Layer: model.ColorLayerTrack,
				Color: model.ColorSpace{
					Primaries: box.ColourPrimaries, Transfer: box.TransferCharacteristics,
					Matrix: box.MatrixCoefficients, FullRange: box.FullRangeFlag, Description: "nclx",
				},
				Origin: "colr/nclx",
			})
		case "nclc":
			// nclc has no full-range flag and carries primaries/transfer/matrix
			// as big-endian uint16.
			if len(box.Unknown) >= 6 {
				cands = append(cands, model.ColorCandidate{
					Layer: model.ColorLayerTrack,
					Color: model.ColorSpace{
						Primaries:   uint16(box.Unknown[0])<<8 | uint16(box.Unknown[1]),
						Transfer:    uint16(box.Unknown[2])<<8 | uint16(box.Unknown[3]),
						Matrix:      uint16(box.Unknown[4])<<8 | uint16(box.Unknown[5]),
						Description: "nclc",
					},
					Origin: "colr/nclc",
				})
			}
		default:
			cands = append(cands, model.ColorCandidate{
				Layer: model.ColorLayerTrack, Color: model.DefaultColor(), Origin: "colr/" + typ,
			})
		}
	}
	// A missing description is recorded explicitly as the unknown default,
	// not invented.
	if len(cands) == 0 {
		cands = append(cands, model.ColorCandidate{
			Layer: model.ColorLayerContainer, Color: model.DefaultColor(), Origin: "absent",
		})
	}
	tr.ColorCandidates = cands
	return nil
}

func firstPayload(r io.ReadSeeker, parent *mp4.BoxInfo, path mp4.BoxPath) (mp4.IBox, error) {
	bs, err := payloads(r, parent, path)
	if err != nil {
		return nil, err
	}
	if len(bs) == 0 {
		return nil, nil
	}
	return bs[0], nil
}

func payloads(r io.ReadSeeker, parent *mp4.BoxInfo, path mp4.BoxPath) ([]mp4.IBox, error) {
	bs, err := mp4.ExtractBoxWithPayload(r, parent, path)
	if err != nil {
		return nil, model.Wrap(model.KindInvalidMedia, "probe.mp4", "extract "+pathName(path), err)
	}
	out := make([]mp4.IBox, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Payload)
	}
	return out, nil
}

func pathName(p mp4.BoxPath) string {
	s := ""
	for i, t := range p {
		if i > 0 {
			s += "/"
		}
		s += t.String()
	}
	return s
}

func chunkOffsets(r io.ReadSeeker, trak *mp4.BoxInfo, trackID uint32) ([]uint64, error) {
	base := mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl()}
	stcos, err := payloads(r, trak, append(append(mp4.BoxPath{}, base...), mp4.BoxTypeStco()))
	if err != nil {
		return nil, err
	}
	if len(stcos) > 0 {
		st, ok := stcos[0].(*mp4.Stco)
		if !ok {
			return nil, model.New(model.KindInvalidMedia, "probe.mp4", "stco payload type mismatch").WithTrack(trackID)
		}
		out := make([]uint64, len(st.ChunkOffset))
		for i, v := range st.ChunkOffset {
			out[i] = uint64(v)
		}
		return out, nil
	}
	co64s, err := payloads(r, trak, append(append(mp4.BoxPath{}, base...), mp4.BoxTypeCo64()))
	if err != nil {
		return nil, err
	}
	if len(co64s) > 0 {
		st, ok := co64s[0].(*mp4.Co64)
		if !ok {
			return nil, model.New(model.KindInvalidMedia, "probe.mp4", "co64 payload type mismatch").WithTrack(trackID)
		}
		return append([]uint64(nil), st.ChunkOffset...), nil
	}
	return nil, model.New(model.KindInvalidMedia, "probe.mp4", "missing stco/co64").WithTrack(trackID)
}
