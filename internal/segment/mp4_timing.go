package segment

import (
	"encoding/binary"
	"fmt"

	"avrepair/internal/media"
)

// ParseMP4 reads fMP4 timing metadata. Truncation is reported through
// MP4Info.Truncated together with whatever was recovered.
func ParseMP4(data []byte) (*MP4Info, error) {
	info := &MP4Info{}
	boxes, err := readBoxes(data)
	if err != nil {
		info.Truncated = true
		if len(boxes) == 0 {
			return nil, err
		}
	}
	if err := info.scanTop(boxes); err != nil {
		return info, err
	}
	if info.Timescale == 0 {
		return info, &FaultTimescale{Detail: "missing mdhd timescale"}
	}
	if info.Timescale < 0 {
		return info, &FaultTimescale{Detail: fmt.Sprintf("negative timescale %d", info.Timescale)}
	}
	return info, nil
}

func (info *MP4Info) scanTop(boxes []box) error {
	var moov, moof []byte
	for _, b := range boxes {
		switch b.typ {
		case "moov":
			moov = b.body
		case "moof":
			moof = b.body
		}
	}
	if moov != nil {
		if err := info.scanMoov(moov); err != nil {
			return err
		}
	}
	if moof != nil {
		if err := info.scanMoof(moof); err != nil {
			return err
		}
	}
	return nil
}

func (info *MP4Info) scanMoov(body []byte) error {
	if mv, ok := findBox(body, "mvhd"); ok && len(mv.body) >= 20 {
		var ts uint32
		if mv.body[0] == 1 {
			if len(mv.body) < 28 {
				return fmt.Errorf("%w: mvhd v1", ErrTruncated)
			}
			ts = binary.BigEndian.Uint32(mv.body[20:24])
		} else {
			ts = binary.BigEndian.Uint32(mv.body[12:16])
		}
		if ts == 0 {
			return &FaultTimescale{Detail: "mvhd timescale is zero"}
		}
		info.Timescale = int64(ts)
	}
	traks, _ := readBoxes(body)
	for _, t := range traks {
		if t.typ != "trak" {
			continue
		}
		tr := MP4Track{}
		if mdia, ok := findBox(t.body, "mdia"); ok {
			if mdhd, ok := findBox(mdia.body, "mdhd"); ok && len(mdhd.body) >= 20 {
				var ts uint32
				if mdhd.body[0] == 1 {
					if len(mdhd.body) < 28 {
						return fmt.Errorf("%w: mdhd v1", ErrTruncated)
					}
					ts = binary.BigEndian.Uint32(mdhd.body[20:24])
				} else {
					ts = binary.BigEndian.Uint32(mdhd.body[12:16])
				}
				if ts == 0 {
					return &FaultTimescale{Detail: "mdhd timescale is zero"}
				}
				if info.Timescale == 0 {
					info.Timescale = int64(ts)
				}
			}
		}
		if tkhd, ok := findBox(t.body, "tkhd"); ok && len(tkhd.body) >= 16 {
			off := 12
			if tkhd.body[0] == 1 {
				off = 20
			}
			if len(tkhd.body) >= off+4 {
				tr.ID = binary.BigEndian.Uint32(tkhd.body[off : off+4])
			}
		}
		info.Tracks = append(info.Tracks, tr)
	}
	return nil
}

func (info *MP4Info) scanMoof(body []byte) error {
	trafs, _ := readBoxes(body)
	for _, t := range trafs {
		if t.typ != "traf" {
			continue
		}
		kids, _ := readBoxes(t.body)
		var trackID, defDur, defSize, defFlags uint32
		for _, k := range kids {
			if k.typ == "tfhd" {
				trackID, defDur, defSize, defFlags = parseTfhd(k.body)
			}
			if k.typ == "trun" {
				samples, err := parseTrun(k.body, defDur, defSize, defFlags)
				if err != nil {
					info.Truncated = true
					continue
				}
				for i := range info.Tracks {
					if info.Tracks[i].ID == trackID {
						info.Tracks[i].Samples += len(samples)
						info.Tracks[i].DefaultDur = defDur
						info.Tracks[i].DefaultSize = defSize
						info.Tracks[i].DefaultFlags = defFlags
					}
				}
			}
		}
	}
	return nil
}

func parseTfhd(body []byte) (trackID, defDur, defSize, defFlags uint32) {
	if len(body) < 8 {
		return 0, 0, 0, 0
	}
	flags := binary.BigEndian.Uint32(body[:4]) & 0x00FFFFFF
	trackID = binary.BigEndian.Uint32(body[4:8])
	off := 8
	if flags&0x01 != 0 {
		off += 8
	}
	if flags&0x02 != 0 {
		off += 4
	}
	if flags&0x08 != 0 && len(body) >= off+4 {
		defDur = binary.BigEndian.Uint32(body[off : off+4])
		off += 4
	}
	if flags&0x10 != 0 && len(body) >= off+4 {
		defSize = binary.BigEndian.Uint32(body[off : off+4])
		off += 4
	}
	if flags&0x20 != 0 && len(body) >= off+4 {
		defFlags = binary.BigEndian.Uint32(body[off : off+4])
	}
	return trackID, defDur, defSize, defFlags
}

func parseTrun(body []byte, defDur, defSize, defFlags uint32) ([]SampleDesc, error) {
	if len(body) < 8 {
		return nil, fmt.Errorf("%w: trun header", ErrTruncated)
	}
	flags := binary.BigEndian.Uint32(body[:4]) & 0x00FFFFFF
	count := int(binary.BigEndian.Uint32(body[4:8]))
	off := 8
	if flags&0x000001 != 0 {
		off += 4
	}
	if flags&0x000004 != 0 {
		off += 4
	}
	perSample := 0
	for _, bit := range []uint32{0x000100, 0x000200, 0x000400, 0x000800} {
		if flags&bit != 0 {
			perSample += 4
		}
	}
	if count < 0 || (perSample > 0 && count > (len(body)-off)/perSample) {
		return nil, fmt.Errorf("%w: trun sample count %d", ErrTruncated, count)
	}
	if perSample == 0 {
		return make([]SampleDesc, 0), nil
	}
	out := make([]SampleDesc, count)
	cur := off
	for i := 0; i < count; i++ {
		sd := SampleDesc{Duration: defDur, Size: defSize, Flags: defFlags}
		if flags&0x000100 != 0 {
			sd.Duration = binary.BigEndian.Uint32(body[cur : cur+4])
			cur += 4
		}
		if flags&0x000200 != 0 {
			sd.Size = binary.BigEndian.Uint32(body[cur : cur+4])
			cur += 4
		}
		if flags&0x000400 != 0 {
			sd.Flags = binary.BigEndian.Uint32(body[cur : cur+4])
			cur += 4
		}
		if flags&0x000800 != 0 {
			sd.Offset = int32(binary.BigEndian.Uint32(body[cur : cur+4]))
			cur += 4
		}
		out[i] = sd
	}
	return out, nil
}

// ParseTfdt returns the base decode time of the first traf found.
func ParseTfdt(data []byte) (int64, error) {
	boxes, err := readBoxes(data)
	if err != nil {
		return 0, err
	}
	for _, b := range boxes {
		if b.typ != "moof" {
			continue
		}
		trafs, _ := readBoxes(b.body)
		for _, t := range trafs {
			if t.typ != "traf" {
				continue
			}
			kids, _ := readBoxes(t.body)
			for _, k := range kids {
				if k.typ != "tfdt" || len(k.body) < 8 {
					continue
				}
				if k.body[0] == 1 {
					if len(k.body) < 12 {
						return 0, fmt.Errorf("%w: tfdt v1", ErrTruncated)
					}
					return int64(binary.BigEndian.Uint64(k.body[4:12])), nil
				}
				return int64(binary.BigEndian.Uint32(k.body[4:8])), nil
			}
		}
	}
	return 0, nil
}

// SamplesFromMP4 derives timeline samples from parsed fMP4 metadata.
// Each trun sample gets a consecutive PTS using tfdt as the base.
func SamplesFromMP4(data []byte, info *MP4Info) ([]media.Sample, error) {
	base, err := ParseTfdt(data)
	if err != nil {
		return nil, err
	}
	var out []media.Sample
	for _, tr := range info.Tracks {
		if tr.Samples == 0 {
			continue
		}
		pts := base
		per := int64(tr.DefaultDur)
		for i := 0; i < tr.Samples; i++ {
			out = append(out, media.Sample{
				PTS:      media.Ticks(pts, info.Timescale),
				Duration: media.Ticks(per, info.Timescale),
				Sync:     i == 0,
				Raw:      pts,
				Scale:    info.Timescale,
			})
			pts += per
		}
	}
	return out, nil
}
