package engine

import (
	"bytes"
	"io"

	mp4 "github.com/abema/go-mp4"

	"github.com/fieldcut/fieldcut/internal/base"
	"github.com/fieldcut/fieldcut/internal/ingest"
)

// ISO BMFF sample flags. Sync samples carry sample_depends_on=1; inter
// samples depend on others. Both are marked as non-redundant, no padding.
const (
	flagSampleNonSync uint32 = 0x01010000
	flagSampleSync    uint32 = 0x02000000
)

// Segment is one keyframe-aligned fragment of one rendition.
type Segment struct {
	Camera    string
	Rendition string
	Codec     ingest.Codec
	Index     int
	StartPTS  uint64
	EndPTS    uint64
	Gap       bool
	Color     ColorTag
	Units     []*ingest.AccessUnit
	Hash      []byte // sha256 of the serialized container, filled on write
	Path      string
}

// colorPriority fixes which color source wins on conflict: an explicit
// policy beats the manifest label, which beats an in-bitstream tag.
var colorPriority = []string{"policy", "manifest", "bitstream"}

// ResolveColor selects one ColorTag from conflicting sources and records the
// winner. Ties follow colorPriority order.
func ResolveColor(tags []ColorTag) (ColorTag, []ColorTag) {
	rank := func(src string) int {
		for i, p := range colorPriority {
			if p == src {
				return i
			}
		}
		return len(colorPriority)
	}
	winner := tags[0]
	best := rank(tags[0].Source)
	for _, t := range tags[1:] {
		if r := rank(t.Source); r < best {
			winner, best = t, r
		}
	}
	return winner, tags
}

// byteSeeker is a bytes.Buffer with Seek, satisfying mp4.NewWriter.
type byteSeeker struct{ buf bytes.Buffer }

func (b *byteSeeker) Write(p []byte) (int, error) { return b.buf.Write(p) }
func (b *byteSeeker) Seek(off int64, whence int) (int64, error) {
	pos := int64(b.buf.Len())
	switch whence {
	case io.SeekStart:
		pos = off
	case io.SeekCurrent:
		pos += off
	case io.SeekEnd:
		pos = int64(b.buf.Len()) + off
	}
	if pos != int64(b.buf.Len()) {
		// The marshaller only seeks to overwrite already-written bytes.
		tmp := b.buf.Bytes()
		b.buf.Reset()
		b.buf.Write(tmp[:pos])
	}
	return pos, nil
}

// WriteSegment serializes one fragment as a standalone MP4: ftyp then a
// single moof/mdat pair. Every sample has explicit duration, size and flags
// so an independent re-parser can reconstruct the exact unit list.
func WriteSegment(w io.Writer, seg *Segment) error {
	bs := &byteSeeker{}
	mw := mp4.NewWriter(bs)

	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()}); err != nil {
		return err
	}
	if err := marshal(mw, &mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 0x200,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', '4'}},
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
		},
	}); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil {
		return err
	}

	trun := buildTrun(seg)
	var payload bytes.Buffer
	for _, u := range seg.Units {
		payload.Write(u.Payload)
	}
	// data_offset is measured from the moof box start: 8-byte mdat header
	// plus the full moof size. Compute moof size via a dry serialization.
	trun.DataOffset = int32(dryMoofSize(seg, trun) + 8)

	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoof()}); err != nil {
		return err
	}
	if err := startEnd(mw, mp4.BoxTypeMfhd(), &mp4.Mfhd{SequenceNumber: uint32(seg.Index + 1)}); err != nil {
		return err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTraf()}); err != nil {
		return err
	}
	if err := startEnd(mw, mp4.BoxTypeTfhd(), &mp4.Tfhd{
		FullBox: mp4.FullBox{Flags: [3]byte{0, 0x02, 0}}, // default-base-is-moof
		TrackID: 1,
	}); err != nil {
		return err
	}
	if err := startEnd(mw, mp4.BoxTypeTfdt(), &mp4.Tfdt{
		FullBox:               mp4.FullBox{Version: 1},
		BaseMediaDecodeTimeV1: seg.StartPTS,
	}); err != nil {
		return err
	}
	if err := startEnd(mw, mp4.BoxTypeTrun(), trun); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil { // traf
		return err
	}
	if _, err := mw.EndBox(); err != nil { // moof
		return err
	}

	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()}); err != nil {
		return err
	}
	if err := marshal(mw, &mp4.Mdat{Data: payload.Bytes()}); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil {
		return err
	}
	_, err := w.Write(bs.buf.Bytes())
	return err
}

func buildTrun(seg *Segment) *mp4.Trun {
	trun := &mp4.Trun{
		FullBox:     mp4.FullBox{Flags: [3]byte{0, 0x0f, 0x01}},
		SampleCount: uint32(len(seg.Units)),
	}
	for i, u := range seg.Units {
		var duration uint32
		if i+1 < len(seg.Units) {
			duration = uint32(seg.Units[i+1].PTS90k - u.PTS90k)
		} else {
			duration = uint32(seg.EndPTS - u.PTS90k)
		}
		flags := flagSampleNonSync
		if u.Keyframe {
			flags = flagSampleSync
		}
		trun.Entries = append(trun.Entries, mp4.TrunEntry{
			SampleDuration: duration, SampleSize: uint32(len(u.Payload)), SampleFlags: flags,
		})
	}
	return trun
}

// dryMoofSize serializes just the moof subtree to measure its byte size.
func dryMoofSize(seg *Segment, trun *mp4.Trun) int {
	dry := &byteSeeker{}
	mw := mp4.NewWriter(dry)
	_, _ = mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoof()})
	_ = startEnd(mw, mp4.BoxTypeMfhd(), &mp4.Mfhd{SequenceNumber: uint32(seg.Index + 1)})
	_, _ = mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTraf()})
	_ = startEnd(mw, mp4.BoxTypeTfhd(), &mp4.Tfhd{
		FullBox: mp4.FullBox{Flags: [3]byte{0, 0x02, 0}}, TrackID: 1,
	})
	_ = startEnd(mw, mp4.BoxTypeTfdt(), &mp4.Tfdt{
		FullBox: mp4.FullBox{Version: 1}, BaseMediaDecodeTimeV1: seg.StartPTS,
	})
	_ = startEnd(mw, mp4.BoxTypeTrun(), trun)
	_, _ = mw.EndBox()
	_, _ = mw.EndBox()
	return dry.buf.Len()
}

func marshal(mw *mp4.Writer, box mp4.IBox) error {
	_, err := mp4.Marshal(mw, box, mp4.Context{})
	return err
}

func startEnd(mw *mp4.Writer, typ mp4.BoxType, box mp4.IBox) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: typ}); err != nil {
		return err
	}
	if err := marshal(mw, box); err != nil {
		return err
	}
	_, err := mw.EndBox()
	return err
}

// SegmentFault builds a boundary fault carrying rendition and clock position.
func SegmentFault(seg *Segment, kind base.FaultKind, format string, args ...any) *base.Fault {
	return base.NewFault(kind, base.Locator{
		Camera: seg.Camera, Rendition: seg.Rendition, PTS: seg.StartPTS,
	}, true, format, args...)
}
