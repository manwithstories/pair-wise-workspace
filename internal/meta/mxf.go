package meta

import (
	"encoding/binary"
	"encoding/hex"
	"math"

	"relaycheck/internal/media"
)

// mxfULPrefix is the 15-byte SMPTE UL prefix used by the convention subset;
// the 16th byte selects the local item tag.
var mxfULPrefix = [15]byte{0x06, 0x0e, 0x2b, 0x34, 0x01, 0x01, 0x01, 0x05, 0x03, 0x01, 0x02, 0x0a, 0x00, 0x00, 0x00}

const (
	mxfTagTrackID        byte = 0x01
	mxfTagTimeScale      byte = 0x02
	mxfTagDuration       byte = 0x03
	mxfTagRotation       byte = 0x10
	mxfTagCrop           byte = 0x11
	mxfTagColor          byte = 0x12
	mxfTagContainerRot   byte = 0x20
	mxfTagContainerCrop  byte = 0x21
	mxfTagContainerColor byte = 0x22
)

func mxfKey(tag byte) [16]byte {
	var k [16]byte
	copy(k[:15], mxfULPrefix[:])
	k[15] = tag
	return k
}

// RawKLV preserves an unknown KLV item verbatim.
type RawKLV struct {
	Key   string `json:"key"`
	Tag   byte   `json:"tag"`
	Value []byte `json:"value"`
}

// MXFTrack is one track's convention-subset claims.
type MXFTrack struct {
	ID        int64
	TimeScale uint32
	Duration  int64
	Rotation  *Rotation
	Crop      *Crop
	Color     *Color
}

// MXFMeta is the parsed KLV metadata.
type MXFMeta struct {
	Track         MXFTrack
	Container     Layer
	Unknown       []RawKLV
	StrictUnknown bool
}

// decodeBERLength decodes an MXF/BER length at off.
func decodeBERLength(data []byte, off int) (uint64, int, *media.Fault) {
	if off >= len(data) {
		return 0, 0, media.NewFault(media.CodeParseError, "missing BER length at %d", off)
	}
	b0 := data[off]
	if b0 < 0x80 {
		return uint64(b0), 1, nil
	}
	n := int(b0 & 0x7f)
	if n == 0 {
		return 0, 1, media.NewFault(media.CodeParseError, "indefinite BER length is not supported")
	}
	if n > 8 {
		return 0, 1, media.NewFault(media.CodeOverflow, "BER length uses %d bytes", n)
	}
	if 1+n > len(data)-off {
		return 0, 1, media.NewFault(media.CodeParseError, "truncated BER long form at %d", off)
	}
	var l uint64
	for i := 0; i < n; i++ {
		l = l<<8 | uint64(data[off+1+i])
	}
	if l > uint64(math.MaxInt64) {
		return 0, 1, media.NewFault(media.CodeOverflow, "BER length overflows int64")
	}
	return l, 1 + n, nil
}

func mxfColor(p []byte) *Color {
	if len(p) < 8 {
		return &Color{Known: false, ColourType: "mxf-short"}
	}
	prim := binary.BigEndian.Uint16(p[0:2])
	trc := binary.BigEndian.Uint16(p[2:4])
	mat := binary.BigEndian.Uint16(p[4:6])
	full := p[6]&0x01 == 1
	c := &Color{FullRange: full, RawMarker: prim}
	pn, pok := primariesNames[prim]
	tn, tok := transferNames[trc]
	mn, mok := matrixNames[mat]
	if pok && tok && mok && p[7] == 1 {
		c.Primaries, c.Transfer, c.Matrix = pn, tn, mn
		c.Known = true
		return c
	}
	c.Primaries = "unknown"
	c.Transfer = "unknown"
	c.Matrix = "unknown"
	c.Known = false
	return c
}

func mxfCrop(p []byte) *Crop {
	if len(p) < 48 {
		return nil
	}
	return &Crop{
		X:      int64(binary.BigEndian.Uint64(p[0:8])),
		Y:      int64(binary.BigEndian.Uint64(p[8:16])),
		W:      int64(binary.BigEndian.Uint64(p[16:24])),
		H:      int64(binary.BigEndian.Uint64(p[24:32])),
		FrameW: int64(binary.BigEndian.Uint64(p[32:40])),
		FrameH: int64(binary.BigEndian.Uint64(p[40:48])),
	}
}

// ReadMXF parses the convention KLV subset, preserving unknown items.
func ReadMXF(data []byte) (*MXFMeta, *media.Fault) {
	return readMXFOptions(data, false)
}

// ReadMXFStrict errors on unknown KLV tags instead of preserving them.
func ReadMXFStrict(data []byte) (*MXFMeta, *media.Fault) {
	return readMXFOptions(data, true)
}

func readMXFOptions(data []byte, strict bool) (m *MXFMeta, ret *media.Fault) {
	defer func() {
		if r := recover(); r != nil {
			ret = media.NewFault(media.CodeParseError, "mxf parser rejected malformed KLV: %v", r)
		}
	}()
	m = &MXFMeta{StrictUnknown: strict}
	off := 0
	for off < len(data) {
		if len(data)-off < 16 {
			return m, media.NewFault(media.CodeParseError, "short KLV key at %d", off).WithTrack(m.Track.ID)
		}
		tag := data[off+15]
		keyHex := hex.EncodeToString(data[off : off+16])
		off += 16
		ln, hlen, f := decodeBERLength(data, off)
		if f != nil {
			return m, f.WithTrack(m.Track.ID)
		}
		off += hlen
		if ln > uint64(len(data)-off) {
			return m, media.NewFault(media.CodeParseError, "KLV tag 0x%02x value length %d overruns", tag, ln).WithTrack(m.Track.ID)
		}
		val := data[off : off+int(ln)]
		off += int(ln)
		if f := m.apply(tag, val, keyHex); f != nil {
			return m, f
		}
	}
	return m, nil
}

func (m *MXFMeta) apply(tag byte, v []byte, keyHex string) *media.Fault {
	switch tag {
	case mxfTagTrackID:
		if len(v) < 4 {
			return media.NewFault(media.CodeParseError, "short track id").WithTrack(m.Track.ID)
		}
		m.Track.ID = int64(binary.BigEndian.Uint32(v))
	case mxfTagTimeScale:
		if len(v) < 4 {
			return media.NewFault(media.CodeParseError, "short timescale").WithTrack(m.Track.ID)
		}
		m.Track.TimeScale = binary.BigEndian.Uint32(v)
	case mxfTagDuration:
		if len(v) < 8 {
			return media.NewFault(media.CodeParseError, "short duration").WithTrack(m.Track.ID)
		}
		m.Track.Duration = int64(binary.BigEndian.Uint64(v))
	case mxfTagRotation:
		if len(v) < 4 {
			return media.NewFault(media.CodeParseError, "short rotation").WithTrack(m.Track.ID)
		}
		r := Rotation{Degrees: int(int32(binary.BigEndian.Uint32(v)))}
		m.Track.Rotation = &r
	case mxfTagCrop:
		if c := mxfCrop(v); c != nil {
			m.Track.Crop = c
		}
	case mxfTagColor:
		m.Track.Color = mxfColor(v)
	case mxfTagContainerRot:
		if len(v) >= 4 {
			r := Rotation{Degrees: int(int32(binary.BigEndian.Uint32(v)))}
			m.Container.Rotation = &r
		}
	case mxfTagContainerCrop:
		m.Container.Crop = mxfCrop(v)
	case mxfTagContainerColor:
		m.Container.Color = mxfColor(v)
	default:
		if m.StrictUnknown {
			return media.NewFault(media.CodeParseError, "unknown MXF KLV tag 0x%02x (%s)", tag, keyHex).WithTrack(m.Track.ID)
		}
		cp := make([]byte, len(v))
		copy(cp, v)
		m.Unknown = append(m.Unknown, RawKLV{Key: keyHex, Tag: tag, Value: cp})
	}
	return nil
}

// Reconcile converts MXF claims into a resolved descriptor.
func (m *MXFMeta) Reconcile(override Layer) (*Descriptor, *media.Fault) {
	d := &Descriptor{Container: "mxf"}
	tr := m.Track
	rt, f := ReconcileTrack(ReconcileInput{
		TrackID: tr.ID, TimeScale: tr.TimeScale, Duration: tr.Duration,
		Override:  override,
		Track:     Layer{Rotation: tr.Rotation, Crop: tr.Crop, Color: tr.Color},
		Container: m.Container,
		Default:   DefaultLayer(),
	})
	if f != nil {
		return d, f
	}
	d.Tracks = append(d.Tracks, rt)
	d.Conflicts = append(d.Conflicts, rt.Conflicts...)
	return d, nil
}
