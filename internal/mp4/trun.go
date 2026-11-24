package mp4

import (
	"encoding/binary"
	"fmt"

	"mediad/internal/core"
)

// parseTrun decodes a track run, strictly bounding every entry read. An
// impossible sample_count is refused before allocation.
func parseTrun(data []byte, h BoxHeader) (trunVersion byte, flags uint32, dataOff int32, hasDO bool, samples []Sample, err error) {
	ver, flags, p, err := ReadFullBox(data, h)
	if err != nil {
		return 0, 0, 0, false, nil, err
	}
	if err = readNeed(data, p, h.End, 4, "trun sample count"); err != nil {
		return
	}
	count := binary.BigEndian.Uint32(data[p : p+4])
	p += 4

	if flags&FlagDataOffsetPresent != 0 {
		if err = readNeed(data, p, h.End, 4, "trun data offset"); err != nil {
			return
		}
		dataOff = int32(binary.BigEndian.Uint32(data[p : p+4]))
		hasDO = true
		p += 4
	}
	var firstFlags uint32
	if flags&FlagFirstSampleFlags != 0 {
		if err = readNeed(data, p, h.End, 4, "trun first sample flags"); err != nil {
			return
		}
		firstFlags = binary.BigEndian.Uint32(data[p : p+4])
		p += 4
	}

	entryWidth := 0
	if flags&FlagSampleDuration != 0 {
		entryWidth += 4
	}
	if flags&FlagSampleSize != 0 {
		entryWidth += 4
	}
	if flags&FlagSampleFlags != 0 {
		entryWidth += 4
	}
	if flags&FlagSampleCTS != 0 {
		entryWidth += 4
	}
	if entryWidth > 0 {
		needBytes := uint64(count) * uint64(entryWidth)
		if needBytes > uint64(h.End-p) {
			return 0, 0, 0, false, nil, core.Faultf(core.KindOutOfBounds, p,
				"trun claims %d samples x %d bytes but only %d remain", count, entryWidth, h.End-p)
		}
	}

	capHint := int(count)
	if capHint > 4096 {
		capHint = 4096
	}
	samples = make([]Sample, 0, capHint)
	for i := uint32(0); i < count; i++ {
		var s Sample
		if flags&FlagSampleDuration != 0 {
			if err = readNeed(data, p, h.End, 4, fmt.Sprintf("trun[%d] duration", i)); err != nil {
				return
			}
			s.Duration = binary.BigEndian.Uint32(data[p : p+4])
			p += 4
		}
		if flags&FlagSampleSize != 0 {
			if err = readNeed(data, p, h.End, 4, fmt.Sprintf("trun[%d] size", i)); err != nil {
				return
			}
			s.Size = binary.BigEndian.Uint32(data[p : p+4])
			p += 4
		}
		if flags&FlagSampleFlags != 0 {
			if err = readNeed(data, p, h.End, 4, fmt.Sprintf("trun[%d] flags", i)); err != nil {
				return
			}
			s.Flags = binary.BigEndian.Uint32(data[p : p+4])
			p += 4
		} else if i == 0 && flags&FlagFirstSampleFlags != 0 {
			s.Flags = firstFlags
		}
		if flags&FlagSampleCTS != 0 {
			if err = readNeed(data, p, h.End, 4, fmt.Sprintf("trun[%d] cts", i)); err != nil {
				return
			}
			raw := binary.BigEndian.Uint32(data[p : p+4])
			if ver == 1 {
				s.CTOffset = int32(raw)
			} else {
				s.CTOffset = int32(raw) // generated v0 offsets stay <= MaxInt32
			}
			p += 4
		}
		samples = append(samples, s)
	}
	return ver, flags, dataOff, hasDO, samples, nil
}
