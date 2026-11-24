package mp4

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"

	gomp4 "github.com/abema/go-mp4"

	"mediarig/internal/media"
)

var containerTypes = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true,
	"stbl": true, "dinf": true, "edts": true, "mvex": true,
	"moof": true, "traf": true,
}

// childBoxStart returns the start of child boxes after fixed box/full-box and
// sample-entry fields. Ordinary containers begin immediately after header.
func childBoxStart(typ string, boxPos, header int64) (int64, bool) {
	if containerTypes[typ] {
		return boxPos + header, true
	}
	switch typ {
	case "stsd":
		// FullBox header bytes are included after the 8-byte box header;
		// skip 4 FullBox bytes plus entry_count.
		return boxPos + header + 8, true
	case "avc1", "avc3", "encv":
		// VisualSampleEntry fixed payload length is 78 bytes.
		return boxPos + header + 78, true
	case "ipcm", "fpcm", "mp4a", "sowt":
		// AudioSampleEntry v0 fixed payload length is 28 bytes.
		return boxPos + header + 28, true
	default:
		return 0, false
	}
}

// ValidateFile scans every box boundary with a deterministic recursive
// scanner and additionally invokes go-mp4's structure reader. It checks
// required boxes, sample counts, durations, and chunk offsets.
func ValidateFile(path string, want MuxConfig) (*Validation, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, media.InternalWrap(media.StageMP4, err, "open mux output for validation")
	}
	defer f.Close()

	if _, err := gomp4.ReadBoxStructure(f, func(h *gomp4.ReadHandle) (any, error) {
		// Returning nil deliberately avoids forcing go-mp4 to decode every
		// vendor/child payload; the boundary scanner below performs the
		// complete recursive size validation.
		return nil, nil
	}); err != nil {
		return nil, media.CorruptWrap(media.StageMP4, err, "go-mp4 box structure read failed")
	}

	info, err := f.Stat()
	if err != nil {
		return nil, media.InternalWrap(media.StageMP4, err, "stat mux output")
	}
	size := info.Size()
	boxes, err := scanBoxes(f, 0, size, "")
	if err != nil {
		return nil, err
	}
	v := &Validation{Boxes: boxes, BoundariesValid: true}
	var hasFtyp, hasMdat, hasMoov bool
	for _, b := range boxes {
		switch b.Type {
		case "ftyp":
			hasFtyp = true
		case "mdat":
			hasMdat = true
			v.MDATSize = b.Size
		case "moov":
			hasMoov = true
		}
	}
	if !hasFtyp || !hasMdat || !hasMoov {
		return nil, media.Corrupt(media.StageMP4, "missing required box ftyp=%v mdat=%v moov=%v", hasFtyp, hasMdat, hasMoov)
	}

	stszCounts, err := parseAllStsz(f, boxes)
	if err != nil {
		return nil, err
	}
	sttsDurations, err := parseAllStts(f, boxes)
	if err != nil {
		return nil, err
	}
	stcoOffsets, err := parseAllStco(f, boxes)
	if err != nil {
		return nil, err
	}
	if len(stszCounts) < 2 || len(sttsDurations) < 2 || len(stcoOffsets) < 2 {
		return nil, media.Corrupt(media.StageMP4, "expected audio and video sample tables, got stsz=%d stts=%d stco=%d",
			len(stszCounts), len(sttsDurations), len(stcoOffsets))
	}
	// First trak is audio, second is video by Mux construction.
	v.AudioSamples = int(stszCounts[0])
	v.VideoSamples = int(stszCounts[1])
	v.AudioDuration = sttsDurations[0]
	v.VideoDuration = sttsDurations[1]
	if v.AudioSamples != len(want.Audio.Samples) {
		return nil, media.Corrupt(media.StageMP4, "audio sample index count %d != %d", v.AudioSamples, len(want.Audio.Samples))
	}
	if v.VideoSamples != len(want.Video.Samples) {
		return nil, media.Corrupt(media.StageMP4, "video sample index count %d != %d", v.VideoSamples, len(want.Video.Samples))
	}
	if want.Audio.Timescale != 0 && v.AudioDuration != uint64(sumDurations(want.Audio.Durations, 960)) {
		return nil, media.Corrupt(media.StageMP4, "audio duration %d does not match sample table", v.AudioDuration)
	}
	if want.Video.Timescale != 0 && v.VideoDuration != uint64(sumVideoDeltas(want.Video.Samples)) {
		return nil, media.Corrupt(media.StageMP4, "video duration %d does not match sample table", v.VideoDuration)
	}
	for _, off := range stcoOffsets {
		if off == 0 || int64(off) >= size {
			return nil, media.Corrupt(media.StageMP4, "invalid chunk offset %d", off)
		}
		if _, err := f.Seek(int64(off), io.SeekStart); err != nil {
			return nil, media.CorruptWrap(media.StageMP4, err, "chunk offset unseekable")
		}
		var one [1]byte
		if _, err := f.Read(one[:]); err != nil {
			return nil, media.Corrupt(media.StageMP4, "chunk offset %d outside mdat", off)
		}
	}
	return v, nil
}

func sumDurations(d []uint32, fallback uint32) uint64 {
	var n uint64
	for _, x := range d {
		if x == 0 {
			x = fallback
		}
		n += uint64(x)
	}
	return n
}
func sumVideoDeltas(s []VideoSample) uint64 {
	var n uint64
	for _, x := range s {
		if x.Delta == 0 {
			n += 3000
		} else {
			n += uint64(x.Delta)
		}
	}
	return n
}

func scanBoxes(r io.ReadSeeker, start, end int64, path string) ([]BoxInfo, error) {
	var out []BoxInfo
	pos := start
	for pos < end {
		if end-pos < 8 {
			return nil, boundaryError(path, pos, "fewer than 8 bytes for box header")
		}
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			return nil, media.InternalWrap(media.StageMP4, err, "seek box header")
		}
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return nil, media.CorruptWrap(media.StageMP4, err, "read box header at %d", pos)
		}
		size64 := uint64(binary.BigEndian.Uint32(hdr[0:4]))
		typ := string(hdr[4:8])
		header := int64(8)
		if size64 == 1 {
			var large [8]byte
			if _, err := io.ReadFull(r, large[:]); err != nil {
				return nil, media.CorruptWrap(media.StageMP4, err, "read largesize for %s", typ)
			}
			size64 = binary.BigEndian.Uint64(large[:])
			header = 16
		} else if size64 == 0 {
			size64 = uint64(end - pos)
		}
		boxEnd := pos + int64(size64)
		if size64 < uint64(header) || boxEnd > end {
			return nil, boundaryError(path+"/"+typ, pos, fmt.Sprintf("illegal box size %d end=%d parent_end=%d", size64, boxEnd, end))
		}
		bi := BoxInfo{Path: path + "/" + typ, Type: typ, Offset: uint64(pos), Size: size64}
		out = append(out, bi)
		if childStart, recursable := childBoxStart(typ, pos, header); recursable {
			children, err := scanBoxes(r, childStart, boxEnd, bi.Path)
			if err != nil {
				return nil, err
			}
			out = append(out, children...)
		}
		pos = boxEnd
	}
	if pos != end {
		return nil, boundaryError(path, pos, "box walk ended before parent end")
	}
	return out, nil
}

func boundaryError(path string, offset int64, msg string) error {
	return media.Corrupt(media.StageMP4, "box boundary invalid at %s offset=%d: %s", path, offset, msg)
}

func parseAllStsz(f *os.File, boxes []BoxInfo) ([]uint32, error) {
	var counts []uint32
	// Parse directly from each stsz BoxInfo: 8-byte box header, 4-byte
	// FullBox header, then sample_size and sample_count.
	for _, b := range boxes {
		if b.Type != "stsz" {
			continue
		}
		if _, err := f.Seek(int64(b.Offset)+12, io.SeekStart); err != nil {
			return nil, err
		}
		var v [8]byte
		if _, err := io.ReadFull(f, v[:]); err != nil {
			return nil, media.CorruptWrap(media.StageMP4, err, "read stsz")
		}
		counts = append(counts, binary.BigEndian.Uint32(v[4:8]))
	}
	return counts, nil
}

func parseAllStts(f *os.File, boxes []BoxInfo) ([]uint64, error) {
	var durs []uint64
	for _, b := range boxes {
		if b.Type != "stts" {
			continue
		}
		if _, err := f.Seek(int64(b.Offset)+8+4, io.SeekStart); err != nil {
			return nil, err
		}
		var ec [4]byte
		if _, err := io.ReadFull(f, ec[:]); err != nil {
			return nil, err
		}
		n := binary.BigEndian.Uint32(ec[:])
		var total uint64
		for i := uint32(0); i < n; i++ {
			var e [8]byte
			if _, err := io.ReadFull(f, e[:]); err != nil {
				return nil, media.CorruptWrap(media.StageMP4, err, "read stts entry")
			}
			c := binary.BigEndian.Uint32(e[0:4])
			d := binary.BigEndian.Uint32(e[4:8])
			total += uint64(c) * uint64(d)
		}
		durs = append(durs, total)
	}
	return durs, nil
}

func parseAllStco(f *os.File, boxes []BoxInfo) ([]uint32, error) {
	var offs []uint32
	for _, b := range boxes {
		if b.Type != "stco" {
			continue
		}
		if _, err := f.Seek(int64(b.Offset)+8+4, io.SeekStart); err != nil {
			return nil, err
		}
		var h [4]byte
		if _, err := io.ReadFull(f, h[:]); err != nil {
			return nil, media.CorruptWrap(media.StageMP4, err, "read stco entry count")
		}
		n := binary.BigEndian.Uint32(h[:])
		for i := uint32(0); i < n; i++ {
			var o [4]byte
			if _, err := io.ReadFull(f, o[:]); err != nil {
				return nil, media.CorruptWrap(media.StageMP4, err, "read stco offset")
			}
			offs = append(offs, binary.BigEndian.Uint32(o[:]))
		}
	}
	return offs, nil
}
