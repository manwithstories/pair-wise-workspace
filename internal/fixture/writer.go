// Package fixture generates small, deterministic CMAF/fMP4 sample batches.
// No external media files are used.
package fixture

import (
	"encoding/binary"
	"os"
	"path/filepath"

	"bcast/internal/media"
)

// SegmentSpec describes one generated segment.
type SegmentSpec struct {
	Path        string
	Sequence    uint32
	TrackID     uint32
	Timescale   uint32
	SampleCount int
	SampleSize  int
	Duration    uint32 // used when Durations is nil
	Durations   []uint32
	CTO         []int32
	TFDT        uint64
}

func boxHeader(typ string, payloadLen int) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b[0:4], uint32(8+payloadLen))
	copy(b[4:8], typ)
	return b
}

func fullBoxHeader(typ string, version byte, flags uint32, fieldLen int) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint32(b[0:4], uint32(12+fieldLen))
	copy(b[4:8], typ)
	b[8] = version
	b[9] = byte(flags >> 16)
	b[10] = byte(flags >> 8)
	b[11] = byte(flags)
	return b
}

func boxWithPayload(typ string, payload []byte) []byte {
	return append(boxHeader(typ, len(payload)), payload...)
}

// WriteInit writes a minimal init.mp4 with moov/trak/tkhd/mdia/mdhd.
func WriteInit(path string, trackID, timescale uint32) error {
	mdhdPayload := make([]byte, 12) // full box header is supplied by fullBoxHeader
	mdhdFields := make([]byte, 12)
	binary.BigEndian.PutUint32(mdhdFields[8:12], timescale) // v0: ... timescale at field offset 8
	mdhd := append(fullBoxHeader("mdhd", 0, 0, len(mdhdFields)), mdhdFields...)
	_ = mdhdPayload

	hdlrFields := make([]byte, 20)
	copy(hdlrFields[4:8], []byte("vide"))
	hdlr := append(fullBoxHeader("hdlr", 0, 0, len(hdlrFields)), hdlrFields...)
	mdia := boxWithPayload("mdia", append(mdhd, hdlr...))

	tkhdFields := make([]byte, 68)
	binary.BigEndian.PutUint32(tkhdFields[8:12], trackID)
	tkhd := append(fullBoxHeader("tkhd", 0, 0x000007, len(tkhdFields)), tkhdFields...)
	trak := boxWithPayload("trak", append(tkhd, mdia...))
	moov := boxWithPayload("moov", trak)

	ftypPayload := []byte("iso5\x00\x00\x02\x00iso5iso6mp41")
	ftyp := boxWithPayload("ftyp", ftypPayload)
	return os.WriteFile(path, append(ftyp, moov...), 0o644)
}

// WriteSegment writes a parseable one-track fMP4 segment plus deterministic
// sample payload.  The trun contains duration, size, sample flags and signed
// composition offset fields.
func WriteSegment(spec SegmentSpec) error {
	if spec.SampleSize < 16 {
		return media.Corrupt("fixture", "sample_size_too_small", spec.Path, spec.Sequence, 0, "sample size must be >= 16")
	}
	if err := os.MkdirAll(filepath.Dir(spec.Path), 0o755); err != nil {
		return media.IO("fixture", spec.Path, spec.Sequence, 0, err)
	}

	trunFields := make([]byte, 8+spec.SampleCount*16)
	binary.BigEndian.PutUint32(trunFields[0:4], uint32(spec.SampleCount))
	dataOffsetField := 4
	pos := 8
	for i := 0; i < spec.SampleCount; i++ {
		d := spec.Duration
		if spec.Durations != nil && i < len(spec.Durations) {
			d = spec.Durations[i]
		}
		binary.BigEndian.PutUint32(trunFields[pos:pos+4], d)
		pos += 4
		binary.BigEndian.PutUint32(trunFields[pos:pos+4], uint32(spec.SampleSize))
		pos += 4
		if i == 0 {
			binary.BigEndian.PutUint32(trunFields[pos:pos+4], 0x02000000)
		} else {
			binary.BigEndian.PutUint32(trunFields[pos:pos+4], 0x01010000)
		}
		pos += 4
		cto := int32(0)
		if spec.CTO != nil && i < len(spec.CTO) {
			cto = spec.CTO[i]
		}
		binary.BigEndian.PutUint32(trunFields[pos:pos+4], uint32(cto))
		pos += 4
	}
	trunFlags := uint32(0x000001 | 0x000100 | 0x000200 | 0x000400 | 0x000800)
	trun := append(fullBoxHeader("trun", 1, trunFlags, len(trunFields)), trunFields...)

	tfhdFields := make([]byte, 4)
	binary.BigEndian.PutUint32(tfhdFields[0:4], spec.TrackID)
	tfhd := append(fullBoxHeader("tfhd", 0, 0x020000, len(tfhdFields)), tfhdFields...)

	tfdtFields := make([]byte, 8)
	binary.BigEndian.PutUint64(tfdtFields[0:8], spec.TFDT)
	tfdt := append(fullBoxHeader("tfdt", 1, 0, len(tfdtFields)), tfdtFields...)

	mfhdFields := make([]byte, 4)
	binary.BigEndian.PutUint32(mfhdFields[0:4], spec.Sequence)
	mfhd := append(fullBoxHeader("mfhd", 0, 0, len(mfhdFields)), mfhdFields...)

	traf := boxWithPayload("traf", append(append(tfhd, tfdt...), trun...))
	moof := boxWithPayload("moof", append(mfhd, traf...))

	// trun data_offset is relative to moof when default-base-is-moof is set.
	// It points immediately after moof and the 8-byte mdat header.
	dataOffset := uint32(len(moof) + 8)
	trunStart := indexOfBox(moof, "trun")
	if trunStart < 0 {
		return media.Corrupt("fixture", "missing_trun_after_build", spec.Path, spec.Sequence, 0, "internal fixture error")
	}
	// Box header(8) + version/flags(4) + sample_count(4) precede data_offset.
	binary.BigEndian.PutUint32(moof[trunStart+16:trunStart+20], dataOffset)
	_ = dataOffsetField

	payloadBytes := spec.SampleCount * spec.SampleSize
	mdat := make([]byte, 8)
	binary.BigEndian.PutUint32(mdat[0:4], uint32(8+payloadBytes))
	copy(mdat[4:8], "mdat")

	f, err := os.Create(spec.Path)
	if err != nil {
		return media.IO("fixture", spec.Path, spec.Sequence, 0, err)
	}
	defer f.Close()
	if _, err := f.Write(moof); err != nil {
		return media.IO("fixture", spec.Path, spec.Sequence, 0, err)
	}
	if _, err := f.Write(mdat); err != nil {
		return media.IO("fixture", spec.Path, spec.Sequence, int64(len(moof)), err)
	}
	zeros := make([]byte, 64<<10)
	for s := 0; s < spec.SampleCount; s++ {
		header := make([]byte, 16)
		binary.BigEndian.PutUint32(header[0:4], spec.Sequence)
		binary.BigEndian.PutUint32(header[4:8], uint32(s))
		binary.BigEndian.PutUint32(header[8:12], uint32(spec.SampleSize))
		if _, err := f.Write(header); err != nil {
			return media.IO("fixture", spec.Path, spec.Sequence, 0, err)
		}
		remaining := spec.SampleSize - 16
		for remaining > 0 {
			n := len(zeros)
			if n > remaining {
				n = remaining
			}
			if _, err := f.Write(zeros[:n]); err != nil {
				return media.IO("fixture", spec.Path, spec.Sequence, 0, err)
			}
			remaining -= n
		}
	}
	return nil
}

// indexOfBox performs a shallow four-character search sufficient for the
// fixture's small, known moof/traf/trun layout.
func indexOfBox(b []byte, typ string) int {
	for i := 0; i+8 <= len(b); i++ {
		if string(b[i+4:i+8]) == typ {
			return i
		}
	}
	return -1
}
