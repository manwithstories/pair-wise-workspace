// Package fixture generates deterministic, fully synthetic MP4 and RTP
// samples: healthy files, damaged files and boundary cases. No external files
// are referenced.
package fixture

import (
	"fmt"
	"io"

	mp4 "github.com/abema/go-mp4"
)

// boxWriter wraps go-mp4's Writer with the small marshal helpers the generator
// needs. It keeps box nesting explicit so every output box is well-formed.
type boxWriter struct {
	w *mp4.Writer
}

func newBoxWriter(w io.WriteSeeker) *boxWriter {
	return &boxWriter{w: mp4.NewWriter(w)}
}

func (b *boxWriter) start(t mp4.BoxType) error {
	_, err := b.w.StartBox(&mp4.BoxInfo{Type: t})
	return err
}

func (b *boxWriter) end() error {
	_, err := b.w.EndBox()
	return err
}

// box writes a complete leaf box: header, marshalled payload, size patch.
func (b *boxWriter) box(box mp4.IImmutableBox) error {
	if _, err := b.w.StartBox(&mp4.BoxInfo{Type: box.GetType()}); err != nil {
		return err
	}
	if err := b.payload(box); err != nil {
		return err
	}
	return b.end()
}

// payload marshals only the box body; the caller owns StartBox/EndBox. Use it
// for boxes that contain child boxes (stsd, dref, sample entries).
func (b *boxWriter) payload(box mp4.IImmutableBox) error {
	if _, err := mp4.Marshal(b.w, box, mp4.Context{}); err != nil {
		return fmt.Errorf("marshal %s: %w", box.GetType().String(), err)
	}
	return nil
}

// markerSampleEntry is the synthetic "mett" entry for the tactical-marker
// track. go-mp4 v1.7 has no built-in mett type, so AnyTypeBox supplies the
// custom four-character code while the fields stay ISO BMFF compliant.
type markerSampleEntry struct {
	mp4.AnyTypeBox
	Reserved           [6]uint8 `mp4:"0,size=8,const=0"`
	DataReferenceIndex uint16   `mp4:"1,size=16"`
	ContentEncoding    string   `mp4:"2,string"`
	MIMEFormat         string   `mp4:"3,string"`
}

func newMarkerSampleEntry() *markerSampleEntry {
	e := &markerSampleEntry{
		DataReferenceIndex: 1,
		ContentEncoding:    "utf-8",
		MIMEFormat:         "text/plain",
	}
	e.SetType(mp4.StrToBoxType("mett"))
	return e
}

func init() {
	// go-mp4 v1.7 has no built-in mett definition; register ours under the
	// custom four-character code so Marshal can resolve the sample entry.
	mp4.AddAnyTypeBoxDef(&markerSampleEntry{}, mp4.StrToBoxType("mett"), 0)
	// nmhd (null media header) is an empty full box go-mp4 does not register.
	mp4.AddBoxDef(&nmhdBox{}, 0)
}

// nmhdBox is the ISO BMFF null media header: a full box with no fields.
type nmhdBox struct {
	mp4.FullBox
}

func (*nmhdBox) GetType() mp4.BoxType { return mp4.StrToBoxType("nmhd") }

func visualEntry(width, height uint16) *mp4.VisualSampleEntry {
	return &mp4.VisualSampleEntry{
		SampleEntry: mp4.SampleEntry{
			AnyTypeBox: mp4.AnyTypeBox{Type: mp4.StrToBoxType("avc1")},
			DataReferenceIndex: 1,
		},
		Width:           width,
		Height:          height,
		Horizresolution: 0x00480000, // 72 dpi fixed point
		Vertresolution:  0x00480000,
		FrameCount:      1,
		Depth:           0x0018,
		PreDefined3:     -1,
	}
}

func audioEntry(channels uint16, rate uint32) *mp4.AudioSampleEntry {
	return &mp4.AudioSampleEntry{
		SampleEntry: mp4.SampleEntry{
			AnyTypeBox: mp4.AnyTypeBox{Type: mp4.StrToBoxType("mp4a")},
			DataReferenceIndex: 1,
		},
		ChannelCount: channels,
		SampleSize:   16,
		SampleRate:   rate << 16, // fixed-point 16.16
	}
}

// colrBox builds an nclx colour box with the given primaries/transfer/matrix.
func colrBox(primaries, transfer, matrix uint16, fullRange bool) *mp4.Colr {
	return &mp4.Colr{
		ColourType:              [4]byte{'n', 'c', 'l', 'x'},
		ColourPrimaries:         primaries,
		TransferCharacteristics: transfer,
		MatrixCoefficients:      matrix,
		FullRangeFlag:           fullRange,
	}
}
