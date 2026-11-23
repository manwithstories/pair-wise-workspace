package mp4

func (r *fileResolver) fillTkhd(td *trackDraft, b rawBox) {
	data, err := readPayload(r.f, b)
	if err != nil || len(data) < 4 {
		return
	}
	version := data[0]
	var id uint32
	var widthOff int
	if version == 1 {
		if len(data) >= 24 {
			id = be32(data[20:])
		}
		widthOff = 4 + 32 + 8 + 8 + 36 // fullbox + v1 times + id/res + streams + matrix
	} else {
		if len(data) >= 16 {
			id = be32(data[12:])
		}
		widthOff = 4 + 20 + 8 + 8 + 36
	}
	td.ID = id
	if widthOff+8 <= len(data) {
		td.Width = int(be32(data[widthOff:]) >> 16)
		td.Height = int(be32(data[widthOff+4:]) >> 16)
	}
}

func (r *fileResolver) fillMdhd(td *trackDraft, b rawBox) {
	data, err := readPayload(r.f, b)
	if err != nil || len(data) < 4 {
		return
	}
	if data[0] == 1 {
		if len(data) >= 28 {
			td.mdhdTimescale = be32(data[20:])
		}
	} else if len(data) >= 16 {
		td.mdhdTimescale = be32(data[12:])
	}
	td.Timescale = td.mdhdTimescale
}

func (r *fileResolver) fillHdlr(td *trackDraft, b rawBox) {
	data, err := readPayload(r.f, b)
	if err != nil || len(data) < 12 {
		return
	}
	handler := string(data[8:12])
	td.Handler = handler
	switch handler {
	case "vide":
		td.Kind = KindVideo
	case "soun":
		td.Kind = KindAudio
	case "sbtl", "text", "meta":
		td.Kind = KindMarker
	default:
		td.Kind = KindUnknown
	}
}

// fillStsd reads the first sample entry to obtain the sample rate of an
// audio track. Dimensions already come from tkhd.
func (r *fileResolver) fillStsd(td *trackDraft, b rawBox) {
	if td.Kind != KindAudio {
		return
	}
	data, err := readPayload(r.f, b)
	if err != nil || len(data) < 8 {
		return
	}
	// fullbox(4) entry_count(4) then the sample entry box (8) and its
	// 28-byte AudioSampleEntry payload; SampleRate sits at entry offset 24.
	const entryPayload = 8 + 8
	if len(data) >= entryPayload+28 {
		td.SampleRate = int(be32(data[entryPayload+24:]) >> 16)
	}
}

func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
