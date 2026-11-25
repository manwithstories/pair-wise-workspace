package ingest

import (
	"encoding/binary"
	"fmt"

	"fieldcut/internal/clock"
)

const (
	MaxMP4Bytes   = 64 << 20
	MaxMP4Samples = 200_000
)

// ColorInfo preserves one video track's color description.
type ColorInfo struct {
	Primaries   string `json:"primaries,omitempty"`
	Transfer    string `json:"transfer,omitempty"`
	Matrix      string `json:"matrix,omitempty"`
	FullRange   bool   `json:"full_range"`
	HasMetadata bool   `json:"has_metadata"`
}

// MP4Sample is one encoded media sample in a generated fragmented MP4.
type MP4Sample struct {
	PTS      clock.Ns `json:"pts_ns"`
	Duration clock.Ns `json:"duration_ns"`
	Data     []byte   `json:"-"`
	Sync     bool     `json:"sync"`
}

// MP4Track is a parsed or to-be-built single-track fragmented MP4.
type MP4Track struct {
	TrackID   int         `json:"track_id"`
	TimeScale uint32      `json:"timescale"`
	Color     ColorInfo   `json:"color"`
	Samples   []MP4Sample `json:"samples"`
}

func boxHeader(boxType string, payloadSize int) []byte {
	size := 8 + payloadSize
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b[0:4], uint32(size))
	copy(b[4:8], boxType)
	return b
}

func makeBox(boxType string, payload []byte) []byte {
	return append(boxHeader(boxType, len(payload)), payload...)
}

func fullBox(version uint8, flags uint32, payload []byte) []byte {
	head := make([]byte, 4)
	head[0] = version
	head[1] = byte(flags >> 16)
	head[2] = byte(flags >> 8)
	head[3] = byte(flags)
	return append(head, payload...)
}

// BuildFragmentedMP4 emits a deterministic moof+mdat subset understood by
// ReadFragmentedMP4 and structurally compatible with common MP4 box headers.
func BuildFragmentedMP4(t *MP4Track) ([]byte, error) {
	if t == nil || t.TimeScale == 0 {
		return nil, clock.Errorf(clock.CodeConfig, trackIDOr(t), "MP4 timescale must be positive")
	}
	if len(t.Samples) == 0 || len(t.Samples) > MaxMP4Samples {
		return nil, clock.Errorf(clock.CodeConfig, t.TrackID, "sample count out of range: %d", len(t.Samples))
	}

	ftypPayload := append([]byte("isom"), 0x00, 0x00, 0x02, 0x00)
	ftypPayload = append(ftypPayload, []byte("isomiso6mp41")...)
	parts := [][]byte{makeBox("ftyp", ftypPayload)}
	tsPayload := make([]byte, 4)
	binary.BigEndian.PutUint32(tsPayload, t.TimeScale)
	parts = append(parts, makeBox("ftsc", tsPayload))
	if t.Color.HasMetadata {
		parts = append(parts, buildColorBox(t.Color))
	}

	baseTicks := uint64(0)
	for start := 0; start < len(t.Samples); {
		end := start + 8
		if end > len(t.Samples) {
			end = len(t.Samples)
		}
		fragment, err := buildFragment(t, t.Samples[start:end], uint64(start/8+1), baseTicks)
		if err != nil {
			return nil, err
		}
		parts = append(parts, fragment)
		dat := make([]byte, 0)
		for _, s := range t.Samples[start:end] {
			dat = append(dat, s.Data...)
			durTicks, err := durationTicks(s.Duration, t.TimeScale)
			if err != nil {
				return nil, err
			}
			baseTicks += durTicks
		}
		parts = append(parts, makeBox("mdat", dat))
		start = end
	}
	out := make([]byte, 0)
	for _, p := range parts {
		out = append(out, p...)
	}
	if len(out) > MaxMP4Bytes {
		return nil, clock.Errorf(clock.CodeConfig, t.TrackID, "generated MP4 exceeds cap")
	}
	return out, nil
}

func trackIDOr(t *MP4Track) int {
	if t == nil {
		return -1
	}
	return t.TrackID
}

func durationTicks(d clock.Ns, timescale uint32) (uint64, error) {
	if d <= 0 {
		return 0, &clock.Error{Code: clock.CodeNegative, TrackID: -1, StartNs: 0, EndNs: d, Message: "sample duration must be positive"}
	}
	// Round nanosecond duration to the track timescale.
	n := uint64(d) * uint64(timescale)
	const denom = uint64(clock.NsPerSecond)
	ticks := (n + denom/2) / denom
	if ticks == 0 {
		ticks = 1
	}
	return ticks, nil
}

func buildColorBox(c ColorInfo) []byte {
	p := make([]byte, 0, 11)
	p = append(p, "nclx"...)
	prim, trans, matrix := colorCodes(c)
	var u16 [2]byte
	binary.BigEndian.PutUint16(u16[:], prim)
	p = append(p, u16[:]...)
	binary.BigEndian.PutUint16(u16[:], trans)
	p = append(p, u16[:]...)
	binary.BigEndian.PutUint16(u16[:], matrix)
	p = append(p, u16[:]...)
	if c.FullRange {
		p = append(p, 0x80)
	} else {
		p = append(p, 0x00)
	}
	return makeBox("colr", p)
}

func colorCodes(c ColorInfo) (primaries, transfer, matrix uint16) {
	switch c.Primaries {
	case "bt2020":
		primaries = 9
	default:
		primaries = 1 // bt709
	}
	switch c.Transfer {
	case "smpte2084":
		transfer = 16
	default:
		transfer = 1 // bt709
	}
	switch c.Matrix {
	case "bt2020nc":
		matrix = 9
	default:
		matrix = 1
	}
	return
}

func buildFragment(t *MP4Track, samples []MP4Sample, seq, baseTicks uint64) ([]byte, error) {
	mfhdPayload := make([]byte, 4)
	binary.BigEndian.PutUint32(mfhdPayload, uint32(seq))
	mfhd := makeBox("mfhd", fullBox(0, 0, mfhdPayload))

	tfhdPayload := make([]byte, 4)
	binary.BigEndian.PutUint32(tfhdPayload, uint32(t.TrackID))
	// flags=0x020000: default-base-is-moof.
	tfhd := makeBox("tfhd", fullBox(0, 0x020000, tfhdPayload))

	tfdtPayload := make([]byte, 8)
	binary.BigEndian.PutUint64(tfdtPayload, baseTicks)
	tfdt := makeBox("tfdt", fullBox(1, 0, tfdtPayload))

	trunPayload := make([]byte, 8+len(samples)*8)
	binary.BigEndian.PutUint32(trunPayload[0:4], uint32(len(samples)))
	// Data offset occupies bytes 4:8 and is filled after moof size is known.
	for i, s := range samples {
		off := 8 + i*8
		dur, err := durationTicks(s.Duration, t.TimeScale)
		if err != nil {
			return nil, err
		}
		binary.BigEndian.PutUint32(trunPayload[off:off+4], uint32(dur))
		binary.BigEndian.PutUint32(trunPayload[off+4:off+8], uint32(len(s.Data)))
	}
	trun := makeBox("trun", fullBox(0, 0x000301, trunPayload)) // data-offset + duration + size
	traf := makeBox("traf", append(append(tfhd, tfdt...), trun...))
	moofPayload := append(mfhd, traf...)
	moof := makeBox("moof", moofPayload)

	// mdat directly follows moof; dataOffset is relative to moof start and
	// includes the eight-byte mdat header.
	dataOffset := uint32(len(moof) + 8)
	// trun box: 8-byte box header, 4-byte FullBox, then sample_count(4),
	// then data_offset(4). Locate it deterministically via suffix layout.
	idx := indexBoxPayload(moof, "trun")
	if idx < 0 {
		return nil, fmt.Errorf("internal error: trun not found")
	}
	// idx points to trun payload (after 8-byte box header). FullBox consumes 4,
	// sample_count consumes 4, data_offset is next.
	binary.BigEndian.PutUint32(moof[idx+8:idx+12], dataOffset)
	return moof, nil
}

func indexBoxPayload(b []byte, boxType string) int {
	marker := []byte(boxType)
	for i := 0; i+4 <= len(b); i++ {
		if b[i] == marker[0] && b[i+1] == marker[1] && b[i+2] == marker[2] && b[i+3] == marker[3] && i >= 4 {
			return i + 4
		}
	}
	return -1
}
