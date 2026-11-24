package mp4

import (
	"encoding/binary"

	"mediad/internal/metadata"
)

// InitSpec describes the init segment fixtures generate.
type InitSpec struct {
	VideoCodec     string // defaults to avc1
	Width, Height  uint16
	VideoTimescale uint32
	Color          *metadata.Color

	AudioTimescale uint32
	Audio          *metadata.Audio
}

func mdhdBox(timescale uint32) []byte {
	body := make([]byte, 16) // creation/modification + timescale + duration
	binary.BigEndian.PutUint32(body[8:12], timescale)
	return appendBox(nil, BoxMdhd, fullBox(0, 0, body))
}

func hdlrBox(handler string) []byte {
	body := make([]byte, 0, 25)
	body = append(body, 0, 0, 0, 0) // pre_defined
	body = append(body, handler...) // 4cc
	body = append(body, make([]byte, 12)...)
	body = append(body, 0) // empty name terminator
	return appendBox(nil, BoxHdlr, fullBox(0, 0, body))
}

func colrBox(c *metadata.Color) []byte {
	if c == nil {
		c = &metadata.Color{ColorType: "nclx", Primaries: 1, Transfer: 1, Matrix: 1}
	}
	if c.ColorType == "" {
		c.ColorType = "nclx"
	}
	p := make([]byte, 0, 11)
	p = append(p, c.ColorType...)
	v := make([]byte, 2)
	binary.BigEndian.PutUint16(v, c.Primaries)
	p = append(p, v...)
	binary.BigEndian.PutUint16(v, c.Transfer)
	p = append(p, v...)
	binary.BigEndian.PutUint16(v, c.Matrix)
	p = append(p, v...)
	flags := byte(0)
	if c.FullRange {
		flags = 0x80
	}
	p = append(p, flags)
	return appendBox(nil, BoxColr, p)
}

func spklBox(a *metadata.Audio) []byte {
	layout := a.Layout
	if len(layout) > 255 {
		layout = layout[:255]
	}
	p := make([]byte, 5+len(layout))
	binary.BigEndian.PutUint32(p[0:4], a.LayoutCode)
	p[4] = byte(len(layout))
	copy(p[5:], layout)
	return appendBox(nil, BoxSpkl, p)
}

func videoSampleEntry(s InitSpec) []byte {
	fixed := make([]byte, 78)
	binary.BigEndian.PutUint16(fixed[6:8], 1) // data_reference_index
	w, h := s.Width, s.Height
	if w == 0 {
		w, h = 1920, 1080
	}
	binary.BigEndian.PutUint16(fixed[24:26], w)
	binary.BigEndian.PutUint16(fixed[26:28], h)
	binary.BigEndian.PutUint32(fixed[28:32], 0x00480000) // 72 dpi h
	binary.BigEndian.PutUint32(fixed[32:36], 0x00480000) // 72 dpi v
	binary.BigEndian.PutUint16(fixed[40:42], 1)          // frame_count
	binary.BigEndian.PutUint16(fixed[74:76], 0x0018)     // depth
	binary.BigEndian.PutUint16(fixed[76:78], 0xFFFF)     // pre_defined
	payload := append(fixed, colrBox(s.Color)...)
	codec := s.VideoCodec
	if codec == "" {
		codec = BoxAvc1
	}
	return appendBox(nil, codec, payload)
}

func audioSampleEntry(a *metadata.Audio) []byte {
	fixed := make([]byte, 28)
	binary.BigEndian.PutUint16(fixed[6:8], 1) // data_reference_index
	ch := a.Channels
	if ch == 0 {
		ch = 2
	}
	binary.BigEndian.PutUint16(fixed[16:18], ch)
	binary.BigEndian.PutUint16(fixed[18:20], 16) // sample size
	rateRaw := a.SampleRateRaw
	if rateRaw == 0 && a.SampleRateHz != 0 {
		rateRaw = a.SampleRateHz << 16
	}
	if rateRaw == 0 {
		rateRaw = 48000 << 16
	}
	binary.BigEndian.PutUint32(fixed[24:28], rateRaw)
	a.SampleRateRaw = rateRaw
	if a.SampleRateHz == 0 {
		a.SampleRateHz = rateRaw >> 16
	}
	payload := append(fixed, spklBox(a)...)
	return appendBox(nil, BoxMp4a, payload)
}

func stsdBox(entry []byte) []byte {
	body := append(u32Body(1), entry...) // entry_count = 1
	return appendBox(nil, BoxStsd, fullBox(0, 0, body))
}

func trakBox(timescale uint32, handler string, entry []byte) []byte {
	stbl := appendBox(nil, BoxStbl, stsdBox(entry))
	minf := appendBox(nil, BoxMinf, stbl)
	mdiaPayload := append(mdhdBox(timescale), hdlrBox(handler)...)
	mdiaPayload = append(mdiaPayload, minf...)
	mdia := appendBox(nil, BoxMdia, mdiaPayload)
	return appendBox(nil, BoxTrak, mdia)
}

// EncodeInit builds a deterministic init segment whose raw metadata can be
// read back by ParseInit.
func EncodeInit(s InitSpec) ([]byte, error) {
	ftypPayload := make([]byte, 0, 20)
	ftypPayload = append(ftypPayload, "iso5"...)
	ftypPayload = append(ftypPayload, 0, 0, 0, 0)
	ftypPayload = append(ftypPayload, "iso5"...)
	ftypPayload = append(ftypPayload, "iso6"...)
	ftypPayload = append(ftypPayload, "mp41"...)
	ftyp := appendBox(nil, BoxFtyp, ftypPayload)

	var traks []byte
	if s.VideoTimescale != 0 || s.Width != 0 || s.Color != nil {
		ts := s.VideoTimescale
		if ts == 0 {
			ts = 90000
		}
		traks = append(traks, trakBox(ts, "vide", videoSampleEntry(s))...)
	}
	if s.Audio != nil {
		ts := s.AudioTimescale
		if ts == 0 {
			ts = s.Audio.SampleRateHz
		}
		if ts == 0 {
			ts = 48000
		}
		traks = append(traks, trakBox(ts, "soun", audioSampleEntry(s.Audio))...)
	}
	moov := appendBox(nil, BoxMoov, traks)
	out := make([]byte, 0, len(ftyp)+len(moov))
	out = append(out, ftyp...)
	out = append(out, moov...)
	return out, nil
}
