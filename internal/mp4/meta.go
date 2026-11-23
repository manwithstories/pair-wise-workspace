package mp4

import (
	"fmt"
	"math"

	"mediasync/internal/model"
)

// readStsd parses the first sample entry for codec and display metadata.
func (r *Reader) readStsd(h hdr, tr *model.Track) error {
	kids, err := r.boxesIn(h.PayloadOffset()+8, h.offset+h.total)
	if err != nil {
		return model.WrapError(model.ErrTruncatedBox, tr.ID, int64(h.offset), err)
	}
	if len(kids) == 0 {
		return model.NewError(model.ErrTruncatedBox, tr.ID, int64(h.offset), "stsd has no sample entries")
	}
	entry := kids[0]
	entryKids, err := r.boxesIn(entryEntryPayloadStart(entry), entry.offset+entry.total)
	if err != nil {
		return model.WrapError(model.ErrTruncatedBox, tr.ID, int64(entry.offset), err)
	}

	switch entry.typ {
	case "avc1", "hvc1", "hev1", "av01", "vp09", "mp4v":
		buf, err := r.payload(entry)
		if err != nil {
			return r.classify(err)
		}
		if len(buf) < 28 {
			return model.NewError(model.ErrTruncatedBox, tr.ID, int64(entry.offset), "visual sample entry payload %d bytes, need 28", len(buf))
		}
		width := int(be16(buf[24:26]))
		height := int(be16(buf[26:28]))
		tr.Codec = entry.typ
		tr.Width = width
		tr.Height = height
		if tr.Meta.SourceWidth == 0 {
			tr.Meta.SourceWidth = width
			tr.Meta.SourceHeight = height
		}
		for _, k := range entryKids {
			switch k.typ {
			case "avcC", "hvcC", "av1C", "vpcC":
				buf, err := r.payload(k)
				if err != nil {
					return r.classify(err)
				}
				tr.CodecConfig = append([]byte(nil), buf...)
			case "colr":
				if err := r.readColr(k, tr); err != nil {
					return err
				}
			case "clap":
				if err := r.readClap(k, tr); err != nil {
					return err
				}
			}
		}
	case "mp4a", "Opus", "fLaC", "ac-3", "ec-3":
		buf, err := r.payload(entry)
		if err != nil {
			return r.classify(err)
		}
		if len(buf) < 28 {
			return model.NewError(model.ErrTruncatedBox, tr.ID, int64(entry.offset), "audio sample entry payload %d bytes, need 28", len(buf))
		}
		if buf[8] == 1 || buf[8] == 2 {
			return model.NewError(model.ErrTruncatedBox, tr.ID, int64(entry.offset), "version %d audio sample entries are not supported", buf[8])
		}
		tr.Codec = entry.typ
		tr.SampleRate = int(be32(buf[24:28]) >> 16)
		tr.Channels = int(be16(buf[16:18]))
		for _, k := range entryKids {
			if k.typ == "esds" {
				buf, err := r.payload(k)
				if err != nil {
					return r.classify(err)
				}
				tr.CodecConfig = append([]byte(nil), buf...)
			}
		}
	default:
		tr.Codec = entry.typ
	}
	return nil
}

// entryEntryPayloadStart skips the fixed sample-entry header for known visual
// and audio entries so display child boxes can be enumerated.
func entryEntryPayloadStart(e hdr) uint64 {
	base := e.PayloadOffset()
	switch e.typ {
	case "avc1", "hvc1", "hev1", "av01", "vp09", "mp4v":
		return base + 78
	case "mp4a", "Opus", "fLaC", "ac-3", "ec-3":
		return base + 28
	default:
		return base + 8
	}
}

// readColr converts a colr (nclx) box into a display claim. Unknown codes are
// preserved and marked.
func (r *Reader) readColr(h hdr, tr *model.Track) error {
	buf, err := r.payload(h)
	if err != nil {
		return r.classify(err)
	}
	if len(buf) < 4 {
		return model.NewError(model.ErrTruncatedBox, tr.ID, int64(h.offset), "colr payload too short")
	}
	kind := string(buf[:4])
	if kind != "nclx" && kind != "prof" && kind != "rICC" {
		tr.Meta.Warnings = append(tr.Meta.Warnings, fmt.Sprintf("colr type %q is not understood; raw bytes preserved", kind))
		tr.Meta.Claims = append(tr.Meta.Claims, model.Claim{
			Field: "colour", Value: kind, Layer: model.LayerTrack, Source: model.SourceMP4, Reason: "unknown colr type",
		})
		return nil
	}
	if kind != "nclx" || len(buf) < 11 {
		tr.Meta.Warnings = append(tr.Meta.Warnings, fmt.Sprintf("colr type %q carries an ICC profile; not interpreted", kind))
		return nil
	}
	c := &model.Colour{
		Primaries: be16(buf[4:6]),
		Transfer:  be16(buf[6:8]),
		Matrix:    be16(buf[8:10]),
		FullRange: buf[10]&0x80 != 0,
		Known:     knownColour(be16(buf[4:6]), be16(buf[6:8]), be16(buf[8:10])),
	}
	if !c.Known {
		c.DefaultInterpretation = "bt709"
		tr.Meta.Warnings = append(tr.Meta.Warnings,
			fmt.Sprintf("unknown colour codes p=%d t=%d m=%d preserved; defaulting interpretation to bt709",
				c.Primaries, c.Transfer, c.Matrix))
	}
	tr.Meta.Colour = c
	tr.Meta.ColourSrc = model.SourceMP4
	tr.Meta.Claims = append(tr.Meta.Claims, model.Claim{
		Field: "colour", Value: fmt.Sprintf("nclx/%d/%d/%d", c.Primaries, c.Transfer, c.Matrix),
		Layer: model.LayerTrack, Source: model.SourceMP4,
	})
	return nil
}

// knownColour reports whether the ISO/IEC 23001-8 codes are recognised.
func knownColour(p, t, m uint16) bool {
	knownPrimaries := map[uint16]bool{1: true, 4: true, 5: true, 6: true, 7: true, 8: true, 9: true, 10: true, 11: true, 12: true, 22: true}
	knownTransfer := map[uint16]bool{1: true, 4: true, 5: true, 6: true, 7: true, 8: true, 9: true, 10: true, 11: true, 12: true, 13: true, 14: true, 15: true, 16: true, 17: true, 18: true}
	knownMatrix := map[uint16]bool{0: true, 1: true, 4: true, 5: true, 6: true, 7: true, 8: true, 9: true, 10: true, 11: true, 12: true, 13: true, 14: true}
	return knownPrimaries[p] && knownTransfer[t] && knownMatrix[m]
}

// readClap converts a clean-aperture box into a crop claim, rejecting crops that
// remove more than the coded frame.
func (r *Reader) readClap(h hdr, tr *model.Track) error {
	buf, err := r.payload(h)
	if err != nil {
		return r.classify(err)
	}
	if len(buf) < 32 {
		return model.NewError(model.ErrTruncatedBox, tr.ID, int64(h.offset), "clap payload too short")
	}
	frac := func(off int) (float64, bool) {
		num := int32(be32(buf[off:]))
		den := int32(be32(buf[off+4:]))
		if den == 0 {
			return 0, false
		}
		return float64(num) / float64(den), true
	}
	cleanW, okW := frac(0)
	cleanH, okH := frac(8)
	if !okW || !okH {
		return model.NewError(model.ErrTruncatedBox, tr.ID, int64(h.offset), "clap has zero denominator")
	}
	w, hgt := tr.Width, tr.Height
	if w == 0 || hgt == 0 {
		w, hgt = tr.Meta.SourceWidth, tr.Meta.SourceHeight
	}
	cropW := float64(w) - cleanW
	cropH := float64(hgt) - cleanH
	if cropW < 0 || cropH < 0 {
		return model.NewError(model.ErrCropOutOfBounds, tr.ID, int64(h.offset),
			"clap clean aperture %.0fx%.0f exceeds coded %dx%d", cleanW, cleanH, w, hgt)
	}
	left := int(math.Round(cropW / 2))
	top := int(math.Round(cropH / 2))
	right := int(math.Round(cropW)) - left
	bottom := int(math.Round(cropH)) - top
	crop := model.Crop{Left: left, Right: right, Top: top, Bottom: bottom}
	tr.Meta.Crop = crop
	tr.Meta.CropSrc = model.SourceMP4
	tr.Meta.Claims = append(tr.Meta.Claims, model.Claim{
		Field: "crop", Value: fmt.Sprintf("%d/%d/%d/%d", left, right, top, bottom),
		Layer: model.LayerTrack, Source: model.SourceMP4,
	})
	return nil
}
