package format

import "medialab/internal/model"

func parseMP4Leaf(data []byte, h MP4BoxHeader, c *Container, t *Track) error {
	if h.Type == "colr" && t == nil {
		col, err := parseCOLR(data, h, 0, "container")
		if err != nil {
			return err
		}
		c.ContainerColor = col
		return nil
	}
	if t == nil {
		return nil
	}
	r := newBoundedReader(data[int(h.Content):int(h.End)], h.Content, model.StageParseMP4, t.ID)
	switch h.Type {
	case "tkhd":
		return parseTKHD(r, t)
	case "mdhd":
		return parseMDHD(r, t)
	case "hdlr":
		return parseHDLR(r, t)
	case "colr":
		col, err := parseCOLR(data, h, t.ID, "track")
		if err != nil {
			return err
		}
		t.Color = col
	case "rott":
		v, err := r.u32("rotation")
		if err != nil {
			return err
		}
		t.Rotation = int(int32(v))
		t.RotationAt = h.Start
	case "crop":
		vals := make([]int64, 4)
		for i := range vals {
			v, err := r.u32("crop_margin")
			if err != nil {
				return err
			}
			vals[i] = int64(int32(v))
		}
		t.Crop = &Crop{Left: vals[0], Top: vals[1], Right: vals[2], Bottom: vals[3], Offset: h.Start}
	case "stss":
		if _, err := r.u32("full_box"); err != nil {
			return err
		}
		count, err := r.u32("sync_sample_count")
		if err != nil {
			return err
		}
		if int(count) > len(r.data)/4 {
			return r.fault("stss_count_out_of_bounds", r.absolute(), errBadBoxLength)
		}
		t.Keyframes = make([]uint32, 0, count)
		for i := uint32(0); i < count; i++ {
			n, err := r.u32("sync_sample")
			if err != nil {
				return err
			}
			t.Keyframes = append(t.Keyframes, n)
		}
	}
	return nil
}

func parseTKHD(r *boundedReader, t *Track) error {
	vf, err := r.u32("version_flags")
	if err != nil {
		return err
	}
	version := byte(vf >> 24)
	if version == 1 {
		if err := r.skip(16, "creation_modification"); err != nil {
			return err
		}
	} else {
		if err := r.skip(8, "creation_modification"); err != nil {
			return err
		}
	}
	id, err := r.u32("track_id")
	if err != nil {
		return err
	}
	t.ID = id
	if version == 1 {
		if err := r.skip(12, "reserved_and_v1_duration"); err != nil { // reserved u32 + duration u64
			return err
		}
	} else {
		if err := r.skip(8, "reserved_and_v0_duration"); err != nil { // reserved u32 + duration u32
			return err
		}
	}
	if err := r.skip(16, "tkhd_layer_alternate_volume_reserved"); err != nil {
		return err
	}
	if err := r.skip(36, "matrix"); err != nil {
		return err
	}
	w, err := r.u32("width")
	if err != nil {
		return err
	}
	h, err := r.u32("height")
	if err != nil {
		return err
	}
	t.Width = w >> 16
	t.Height = h >> 16
	return nil
}

func parseMDHD(r *boundedReader, t *Track) error {
	vf, err := r.u32("version_flags")
	if err != nil {
		return err
	}
	if byte(vf>>24) == 1 {
		if err := r.skip(16, "creation_modification"); err != nil {
			return err
		}
		ts, err := r.u32("timescale")
		if err != nil {
			return err
		}
		dur, err := r.u64("duration")
		if err != nil {
			return err
		}
		t.Timescale, t.Duration = ts, dur
		return nil
	}
	if err := r.skip(8, "creation_modification"); err != nil {
		return err
	}
	ts, err := r.u32("timescale")
	if err != nil {
		return err
	}
	dur, err := r.u32("duration")
	if err != nil {
		return err
	}
	t.Timescale, t.Duration = ts, uint64(dur)
	return nil
}

func parseHDLR(r *boundedReader, t *Track) error {
	if _, err := r.u32("full_box"); err != nil {
		return err
	}
	if _, err := r.u32("predefined"); err != nil {
		return err
	}
	h, err := r.bytes(4, "handler_type")
	if err != nil {
		return err
	}
	t.Handler = string(h)
	switch t.Handler {
	case "vide":
		t.Kind = KindVideo
	case "soun":
		t.Kind = KindAudio
	default:
		t.Kind = KindData
	}
	return nil
}

func parseCOLR(data []byte, h MP4BoxHeader, track uint32, source string) (*ColorDescription, error) {
	// Color boxes are fixed enough to decode directly at absolute offsets.
	base := int(h.Content)
	if h.End-h.Content < 4 {
		return nil, model.New(model.StageParseMP4, "truncated_colr", track, h.Start, errTruncated)
	}
	colorType := string(data[base : base+4])
	if colorType != "nclx" {
		return &ColorDescription{Primaries: colorType, Source: source, Offset: h.Start}, nil
	}
	if h.End-h.Content < 11 { // type + 3 u16 + range byte minimum
		return nil, model.New(model.StageParseMP4, "truncated_nclx", track, h.Start, errTruncated)
	}
	p := uint16(data[base+4])<<8 | uint16(data[base+5])
	tr := uint16(data[base+6])<<8 | uint16(data[base+7])
	m := uint16(data[base+8])<<8 | uint16(data[base+9])
	return &ColorDescription{
		Primaries: colorStandard(p),
		Transfer:  colorStandard(tr),
		Matrix:    colorStandard(m),
		Source:    source,
		Offset:    h.Start,
	}, nil
}

func colorStandard(code uint16) string {
	switch code {
	case 1:
		return "bt709"
	case 6:
		return "smpte170m"
	case 9:
		return "bt2020"
	case 16:
		return "smpte2084"
	default:
		return "unknown"
	}
}
