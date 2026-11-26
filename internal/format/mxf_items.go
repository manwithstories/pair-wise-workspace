package format

import "medialab/internal/model"

func parseMXFItem(key MXFKey, value []byte, klvOffset, valueOffset int64, c *Container, current *Track) (*Track, error) {
	tag := key[15]
	if tag == MXFTagTrackID {
		id, err := mxfUint32(value, valueOffset, currentTrackID(current))
		if err != nil {
			return current, err
		}
		for _, tr := range c.Tracks {
			if tr.ID == id {
				return tr, nil
			}
		}
		tr := &Track{ID: id, DataOffset: klvOffset}
		c.Tracks = append(c.Tracks, tr)
		return tr, nil
	}
	if tag == MXFTagContainerColor {
		col, err := mxfColor(value, valueOffset, 0, "container", klvOffset)
		if err != nil {
			return current, err
		}
		c.ContainerColor = col
		return current, nil
	}
	if current == nil {
		return nil, model.New(model.StageParseMXF, "item_before_track_id", 0, klvOffset, errUnsupported)
	}
	r := newBoundedReader(value, valueOffset, model.StageParseMXF, current.ID)
	switch tag {
	case MXFTagKind:
		b, err := r.bytes(4, "kind")
		if err != nil {
			return current, err
		}
		current.Handler = string(b)
		switch current.Handler {
		case "vide":
			current.Kind = KindVideo
		case "soun":
			current.Kind = KindAudio
		default:
			current.Kind = KindData
		}
	case MXFTagTimescale:
		v, err := r.u32("timescale")
		if err != nil {
			return current, err
		}
		current.Timescale = v
	case MXFTagDuration:
		v, err := r.u64("duration")
		if err != nil {
			return current, err
		}
		current.Duration = v
	case MXFTagDimensions:
		w, err := r.u32("width")
		if err != nil {
			return current, err
		}
		h, err := r.u32("height")
		if err != nil {
			return current, err
		}
		current.Width, current.Height = w, h
	case MXFTagRotation:
		v, err := r.u32("rotation")
		if err != nil {
			return current, err
		}
		current.Rotation = int(int32(v))
		current.RotationAt = klvOffset
	case MXFTagCrop:
		vals := make([]int64, 4)
		for i := range vals {
			v, err := r.u32("crop_margin")
			if err != nil {
				return current, err
			}
			vals[i] = int64(int32(v))
		}
		current.Crop = &Crop{Left: vals[0], Top: vals[1], Right: vals[2], Bottom: vals[3], Offset: klvOffset}
	case MXFTagTrackColor:
		col, err := mxfColor(value, valueOffset, current.ID, "track", klvOffset)
		if err != nil {
			return current, err
		}
		current.Color = col
	case MXFTagKeyframes:
		if len(value)%4 != 0 {
			return current, model.New(model.StageParseMXF, "bad_keyframe_list", current.ID, valueOffset, errTruncated)
		}
		current.Keyframes = make([]uint32, 0, len(value)/4)
		for len(r.data) > r.pos {
			n, err := r.u32("keyframe")
			if err != nil {
				return current, err
			}
			current.Keyframes = append(current.Keyframes, n)
		}
	default:
		// Unknown keys are part of the wider MXF specification but outside
		// this implementation's tested metadata subset; preserve forward flow.
	}
	return current, nil
}

func mxfColor(value []byte, offset int64, track uint32, source string, klvOffset int64) (*ColorDescription, error) {
	if len(value) < 6 {
		return nil, model.New(model.StageParseMXF, "truncated_color", track, offset, errTruncated)
	}
	p := uint16(value[0])<<8 | uint16(value[1])
	tr := uint16(value[2])<<8 | uint16(value[3])
	m := uint16(value[4])<<8 | uint16(value[5])
	return &ColorDescription{
		Primaries: colorStandard(p),
		Transfer:  colorStandard(tr),
		Matrix:    colorStandard(m),
		Source:    source,
		Offset:    klvOffset,
	}, nil
}

// Uint32Value encodes a uint32 KLV value.
func Uint32Value(v uint32) []byte {
	return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}

// Uint64Value encodes a uint64 KLV value.
func Uint64Value(v uint64) []byte {
	out := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		out[i] = byte(v & 0xff)
		v >>= 8
	}
	return out
}

// Int32Value encodes a signed int32 KLV value.
func Int32Value(v int32) []byte { return Uint32Value(uint32(v)) }

// ColorValue encodes three NCLX-style code points.
func ColorValue(primaries, transfer, matrix uint16) []byte {
	return []byte{
		byte(primaries >> 8), byte(primaries),
		byte(transfer >> 8), byte(transfer),
		byte(matrix >> 8), byte(matrix),
	}
}
