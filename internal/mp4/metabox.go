package mp4

import (
	"encoding/binary"

	"mediad/internal/core"
	"mediad/internal/metadata"
)

// TrackInfo is the raw metadata extracted from one init-segment track.
type TrackInfo struct {
	Handler   string // "vide" or "soun"
	Timescale uint32
	Width     uint16
	Height    uint16
	Color     *metadata.Color
	Audio     *metadata.Audio
}

// InitInfo contains all tracks parsed from an init segment.
type InitInfo struct {
	Tracks []TrackInfo
}

// ListChildren enumerates direct child boxes with parent-bound checks.
func ListChildren(data []byte, parent BoxHeader) ([]BoxHeader, error) {
	var out []BoxHeader
	off := parent.PayloadOff
	for off < parent.End {
		h, err := ReadHeader(data, off)
		if err != nil {
			return nil, err
		}
		if h.End > parent.End {
			return nil, core.Faultf(core.KindOutOfBounds, off,
				"child %q end %d exceeds parent %q end %d", h.Type, h.End, parent.Type, parent.End)
		}
		out = append(out, h)
		off = h.End
	}
	return out, nil
}

func parseMdhdTimescale(data []byte, h BoxHeader) (uint32, error) {
	ver, _, p, err := ReadFullBox(data, h)
	if err != nil {
		return 0, err
	}
	if ver == 1 {
		if err := readNeed(data, p, h.End, 20, "mdhd v1 timescale"); err != nil { // 16 times + 4 scale
			return 0, err
		}
		return binary.BigEndian.Uint32(data[p+16 : p+20]), nil
	}
	if err := readNeed(data, p, h.End, 12, "mdhd timescale"); err != nil { // 8 times + 4 scale
		return 0, err
	}
	return binary.BigEndian.Uint32(data[p+8 : p+12]), nil
}

func parseHdlr(data []byte, h BoxHeader) (string, error) {
	_, _, p, err := ReadFullBox(data, h)
	if err != nil {
		return "", err
	}
	if err := readNeed(data, p, h.End, 8, "hdlr handler type"); err != nil {
		return "", err
	}
	return string(data[p+4 : p+8]), nil
}

func parseColr(data []byte, h BoxHeader) (*metadata.Color, error) {
	if err := readNeed(data, h.PayloadOff, h.End, 4, "colr type"); err != nil {
		return nil, err
	}
	c := &metadata.Color{ColorType: string(data[h.PayloadOff : h.PayloadOff+4])}
	if c.ColorType != "nclx" {
		return c, nil // preserve unsupported colour boxes by type only
	}
	p := h.PayloadOff + 4
	if err := readNeed(data, p, h.End, 7, "colr nclx fields"); err != nil { // 2+2+2+1
		return nil, err
	}
	c.Primaries = binary.BigEndian.Uint16(data[p : p+2])
	c.Transfer = binary.BigEndian.Uint16(data[p+2 : p+4])
	c.Matrix = binary.BigEndian.Uint16(data[p+4 : p+6])
	c.FullRange = data[p+6]&0x80 != 0
	return c, nil
}

func parseSpkl(data []byte, h BoxHeader) (code uint32, layout string, err error) {
	if err = readNeed(data, h.PayloadOff, h.End, 5, "spkl header"); err != nil {
		return 0, "", err
	}
	p := h.PayloadOff
	code = binary.BigEndian.Uint32(data[p : p+4])
	n := int(data[p+4])
	if err = readNeed(data, p+5, h.End, n, "spkl layout"); err != nil {
		return 0, "", err
	}
	layout = string(data[p+5 : p+5+int64(n)])
	return code, layout, nil
}

func parseVisualEntry(data []byte, entry BoxHeader, t *TrackInfo) error {
	// width/height live at fixed offsets 24/26 inside the sample-entry payload.
	if err := readNeed(data, entry.PayloadOff, entry.End, 28, "visual sample entry"); err != nil {
		return err
	}
	t.Width = binary.BigEndian.Uint16(data[entry.PayloadOff+24 : entry.PayloadOff+26])
	t.Height = binary.BigEndian.Uint16(data[entry.PayloadOff+26 : entry.PayloadOff+28])
	kids, err := ListChildren(data, BoxHeader{
		Type:       entry.Type,
		Offset:     entry.Offset,
		PayloadOff: entry.PayloadOff + 78, // VisualSampleEntry fixed fields
		End:        entry.End,
	})
	if err != nil {
		return err
	}
	for _, k := range kids {
		if k.Type == BoxColr {
			if t.Color, err = parseColr(data, k); err != nil {
				return err
			}
		}
	}
	return nil
}

func parseAudioEntry(data []byte, entry BoxHeader, t *TrackInfo) error {
	if err := readNeed(data, entry.PayloadOff, entry.End, 28, "audio sample entry"); err != nil {
		return err
	}
	a := &metadata.Audio{}
	a.Channels = binary.BigEndian.Uint16(data[entry.PayloadOff+16 : entry.PayloadOff+18])
	a.SampleRateRaw = binary.BigEndian.Uint32(data[entry.PayloadOff+24 : entry.PayloadOff+28])
	a.SampleRateHz = a.SampleRateRaw >> 16
	kids, err := ListChildren(data, BoxHeader{
		Type:       entry.Type,
		Offset:     entry.Offset,
		PayloadOff: entry.PayloadOff + 28, // AudioSampleEntry v0 fixed fields
		End:        entry.End,
	})
	if err != nil {
		return err
	}
	for _, k := range kids {
		if k.Type == BoxSpkl {
			code, layout, err := parseSpkl(data, k)
			if err != nil {
				return err
			}
			a.LayoutCode = code
			a.Layout = layout
		}
	}
	t.Audio = a
	return nil
}

func parseTrak(data []byte, trak BoxHeader) (TrackInfo, error) {
	var t TrackInfo
	mdia, ok, err := FindDirectChild(data, trak, BoxMdia)
	if err != nil || !ok {
		return t, err
	}
	if mdhd, found, ferr := FindDirectChild(data, mdia, BoxMdhd); ferr != nil {
		return t, ferr
	} else if found {
		if t.Timescale, err = parseMdhdTimescale(data, mdhd); err != nil {
			return t, err
		}
	}
	if hdlr, found, ferr := FindDirectChild(data, mdia, BoxHdlr); ferr != nil {
		return t, ferr
	} else if found {
		if t.Handler, err = parseHdlr(data, hdlr); err != nil {
			return t, err
		}
	}
	stsdH, found, err := FindPath(data, mdia, BoxMinf, BoxStbl, BoxStsd)
	if err != nil {
		return t, err
	}
	if !found {
		return t, nil
	}
	_, _, p, err := ReadFullBox(data, stsdH)
	if err != nil {
		return t, err
	}
	if err := readNeed(data, p, stsdH.End, 4, "stsd entry_count"); err != nil {
		return t, err
	}
	count := binary.BigEndian.Uint32(data[p : p+4])
	if count == 0 {
		return t, nil
	}
	entry, err := ReadHeader(data, p+4)
	if err != nil {
		return t, err
	}
	if entry.End > stsdH.End {
		return t, core.Faultf(core.KindOutOfBounds, entry.Offset, "sample entry escapes stsd")
	}
	switch entry.Type {
	case BoxAvc1, BoxHvc1:
		if err := parseVisualEntry(data, entry, &t); err != nil {
			return t, err
		}
	case BoxMp4a:
		if err := parseAudioEntry(data, entry, &t); err != nil {
			return t, err
		}
	}
	return t, nil
}

// ParseInit parses an ftyp+moov init segment and extracts track metadata.
func ParseInit(data []byte) (*InitInfo, error) {
	var moov BoxHeader
	found := false
	if err := WalkTop(data, func(h BoxHeader) error {
		if h.Type == BoxMoov && !found {
			moov = h
			found = true
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if !found {
		return nil, core.Faultf(core.KindBadField, 0, "init segment has no moov")
	}
	traks, err := ListChildren(data, moov)
	if err != nil {
		return nil, err
	}
	info := &InitInfo{}
	for _, tk := range traks {
		if tk.Type != BoxTrak {
			continue
		}
		tr, err := parseTrak(data, tk)
		if err != nil {
			return nil, err
		}
		info.Tracks = append(info.Tracks, tr)
	}
	return info, nil
}

// Video returns the first video track info and whether present.
func (i *InitInfo) Video() (TrackInfo, bool) {
	for _, t := range i.Tracks {
		if t.Handler == "vide" || t.Color != nil {
			return t, true
		}
	}
	return TrackInfo{}, false
}

// Audio returns the first audio track info.
func (i *InitInfo) Audio() (TrackInfo, bool) {
	for _, t := range i.Tracks {
		if t.Handler == "soun" || t.Audio != nil {
			return t, true
		}
	}
	return TrackInfo{}, false
}
