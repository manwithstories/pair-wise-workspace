package mp4

import (
	"io"

	"github.com/abema/go-mp4"
)

// BoxTypeWvtt is a WebVTT sample entry, registered so fragmented subtitle
// tracks round-trip through go-mp4.
func BoxTypeWvtt() mp4.BoxType { return mp4.StrToBoxType("wvtt") }

// WvttSampleEntry is a minimal wvtt sample entry (payload is opaque to us).
type WvttSampleEntry struct {
	mp4.SampleEntry `mp4:"0,extend"`
}

// GetType returns the box type.
func (*WvttSampleEntry) GetType() mp4.BoxType { return BoxTypeWvtt() }

// Nmhd is the null media header box used for subtitle tracks; go-mp4
// v1.0.0 does not define it, so it is registered here.
type Nmhd struct {
	mp4.FullBox `mp4:"0,extend"`
}

// GetType returns the box type.
func (*Nmhd) GetType() mp4.BoxType { return mp4.StrToBoxType("nmhd") }

func init() {
	mp4.AddAnyTypeBoxDef(&WvttSampleEntry{}, BoxTypeWvtt())
	mp4.AddBoxDef(&Nmhd{})
}

// MuxTrack describes one output track for the fragmented muxer.
type MuxTrack struct {
	ID        uint32
	Type      string // "video", "audio", "subtitle"
	Codec     string // "avc1", "mp4a", "wvtt"
	Timescale uint32
	Language  string // ISO-639-2/T, may be empty
	Width     uint16
	Height    uint16
}

// FragTrack carries the samples of one track within one fragment. DTS is
// in the track timescale and may exceed 32 bits (tfdt v1 is used unless
// the muxer was created with Tfdt32 for wrap-around fixtures).
type FragTrack struct {
	Samples []Sample
}

// Muxer writes fMP4: ftyp+moov at creation, one moof+mdat per fragment.
type Muxer struct {
	out    io.WriteSeeker
	w      *mp4.Writer
	tracks []MuxTrack
	seq    uint32
	Tfdt32 bool // emit 32-bit tfdt v0 (for wrap-around test fixtures)
}

var stdMatrix = [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}

// NewMuxer writes ftyp and moov for the given tracks.
func NewMuxer(out io.WriteSeeker, tracks []MuxTrack) (*Muxer, error) {
	m := &Muxer{out: out, w: mp4.NewWriter(out), tracks: tracks, seq: 1}
	if err := m.writeHeader(); err != nil {
		return nil, err
	}
	return m, nil
}

// ResumeMuxer continues an existing fMP4 file (header already written):
// no ftyp/moov is emitted and fragment numbering continues at seq.
func ResumeMuxer(out io.WriteSeeker, tracks []MuxTrack, seq uint32) *Muxer {
	return &Muxer{out: out, w: mp4.NewWriter(out), tracks: tracks, seq: seq}
}

// Offset returns the current write position.
func (m *Muxer) Offset() int64 {
	off, _ := m.out.Seek(0, io.SeekCurrent)
	return off
}

func (m *Muxer) box(t mp4.BoxType, payload mp4.IImmutableBox) error {
	if _, err := m.w.StartBox(&mp4.BoxInfo{Type: t}); err != nil {
		return err
	}
	if payload != nil {
		if _, err := mp4.Marshal(m.w, payload, mp4.Context{}); err != nil {
			return err
		}
	}
	_, err := m.w.EndBox()
	return err
}

// with runs fn inside a container box.
func (m *Muxer) with(t mp4.BoxType, fn func() error) error {
	if _, err := m.w.StartBox(&mp4.BoxInfo{Type: t}); err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	_, err := m.w.EndBox()
	return err
}

func (m *Muxer) writeHeader() error {
	err := m.box(mp4.BoxTypeFtyp(), &mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', '6'},
		MinorVersion: 1,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', '6'}},
			{CompatibleBrand: [4]byte{'m', 'p', '4', '1'}},
			{CompatibleBrand: [4]byte{'d', 'a', 's', 'h'}},
		},
	})
	if err != nil {
		return err
	}
	return m.with(mp4.BoxTypeMoov(), func() error {
		if err := m.box(mp4.BoxTypeMvhd(), &mp4.Mvhd{
			Timescale: 1000, Rate: 0x10000, Volume: 0x100,
			Matrix: stdMatrix, NextTrackID: uint32(len(m.tracks)) + 1,
		}); err != nil {
			return err
		}
		for _, t := range m.tracks {
			if err := m.writeTrak(t); err != nil {
				return err
			}
		}
		return m.with(mp4.BoxTypeMvex(), func() error {
			for _, t := range m.tracks {
				if err := m.box(mp4.BoxTypeTrex(), &mp4.Trex{
					TrackID: t.ID, DefaultSampleDescriptionIndex: 1,
				}); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

func (m *Muxer) writeTrak(t MuxTrack) error {
	return m.with(mp4.BoxTypeTrak(), func() error {
		tk := &mp4.Tkhd{TrackID: t.ID, Matrix: stdMatrix,
			Width: uint32(t.Width) << 16, Height: uint32(t.Height) << 16}
		tk.Flags[0], tk.Flags[1], tk.Flags[2] = 0, 0, 0x07 // enabled|in_movie|in_preview
		if t.Type == "audio" {
			tk.Volume = 0x100
		}
		if err := m.box(mp4.BoxTypeTkhd(), tk); err != nil {
			return err
		}
		return m.with(mp4.BoxTypeMdia(), func() error {
			md := &mp4.Mdhd{Timescale: t.Timescale}
			copy(md.Language[:], langBytes(t.Language))
			if err := m.box(mp4.BoxTypeMdhd(), md); err != nil {
				return err
			}
			h := &mp4.Hdlr{Name: t.Type + " handler"}
			switch t.Type {
			case "video":
				h.HandlerType = [4]byte{'v', 'i', 'd', 'e'}
			case "audio":
				h.HandlerType = [4]byte{'s', 'o', 'u', 'n'}
			default:
				h.HandlerType = [4]byte{'s', 'u', 'b', 't'}
			}
			if err := m.box(mp4.BoxTypeHdlr(), h); err != nil {
				return err
			}
			return m.with(mp4.BoxTypeMinf(), m.writeMinf(t))
		})
	})
}

// langBytes encodes an ISO-639-2/T code as go-mp4's 5-bit packed bytes
// (each letter stored as c - 0x60).
func langBytes(s string) []byte {
	b := []byte(s)
	if len(b) > 3 {
		b = b[:3]
	}
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			out[i] = c - 0x60
		}
	}
	return out
}
