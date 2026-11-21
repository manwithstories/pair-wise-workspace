package probe

import (
	"io"

	"github.com/abema/go-mp4"

	"mediad/internal/clock"
	"mediad/internal/model"
)

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
		switch len(h.Path) {
		case 1: // the stsd box itself: descend into its sample entries
			_, err := h.Expand()
			return nil, err
		case 2: // first child is the sample entry
			if entry == nil {
				cp := h.BoxInfo
				entry = &cp
			}
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

func rotationAndCrop(r io.ReadSeeker, trak *mp4.BoxInfo, th *mp4.Tkhd, stsd *mp4.BoxInfo, tr *model.Track, pendingCropErr *error) error {
	rot, err := rotationFromMatrix(th.Matrix)
	if err != nil {
		return model.Wrap(model.KindInvalidMedia, "probe.mp4", "rotation", err).WithTrack(tr.ID)
	}
	tr.Rotation = rot
	tr.Crop = model.Crop{}

	// clap is not modelled by go-mp4 v1.4.1; read the raw payload.
	claps, err := mp4.ExtractBox(r, stsd, mp4.BoxPath{mp4.BoxTypeAny(), mp4.StrToBoxType("clap")})
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
		// Range validation happens once samples exist, so the error can name
		// the track's time range as well as its ID.
		crop, err := clap.crop(tr.CodedWidth, tr.CodedHeight)
		if err != nil {
			*pendingCropErr = err
			return nil
		}
		tr.Crop = crop
	}

	pasps, err := mp4.ExtractBoxWithPayload(r, stsd, mp4.BoxPath{mp4.BoxTypeAny(), mp4.BoxTypePasp()})
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
	colrs, err := mp4.ExtractBoxWithPayload(r, stsd, mp4.BoxPath{mp4.BoxTypeAny(), mp4.BoxTypeColr()})
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
