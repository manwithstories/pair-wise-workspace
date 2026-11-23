package output

import (
	"encoding/binary"
	"fmt"

	"bcast/media"

	mp4 "github.com/abema/go-mp4"
)

// Default coded dimensions for synthesized sample entries. Fixture video is
// not really decodable, so these are structural placeholders; supplier files
// carry their own stsd which is copied verbatim instead.
const (
	defaultWidth  = 640
	defaultHeight = 360
)

// writeStsd emits one sample-description box. A track that came from a
// source file carries its stsd bytes and they are written unchanged, so
// codec configuration (SPS/PPS, audio config) survives the remux exactly.
// Synthesized entries exist only for the fixture generator.
func writeStsd(mw *mp4.Writer, t TrackMux) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsd()}); err != nil {
		return err
	}
	// FullBox version/flags (0) followed by EntryCount.
	var fixed [8]byte
	binary.BigEndian.PutUint32(fixed[4:], 1)
	if _, err := mw.Write(fixed[:]); err != nil {
		return err
	}
	if len(t.CodecEntry) > 0 {
		if _, err := mw.Write(t.CodecEntry); err != nil {
			return fmt.Errorf("output: copy stsd for track %d: %w", t.ID, err)
		}
	} else if err := writeSynthEntry(mw, t.Kind); err != nil {
		return err
	}
	_, err := mw.EndBox()
	return err
}

func writeSynthEntry(mw *mp4.Writer, kind media.TrackKind) error {
	if kind == media.TrackAudio {
		return writeSynthAudio(mw)
	}
	return writeSynthVideo(mw)
}

// avcCPayload is a structurally valid AVC decoder configuration record with
// one SPS and one PPS. The NAL bytes are placeholders; consumers that need
// real codec data use the source stsd instead.
var avcC = []byte{
	1,                // configurationVersion
	0x42, 0xc0, 0x1f, // profile, compatibility, level
	0xff,       // reserved(6) | lengthSizeMinusOne(2)=3
	0xe1,       // reserved(3) | numOfSPS=1
	0x00, 0x04, // SPS length
	0x67, 0x42, 0xc0, 0x1f,
	1,          // numOfPPS
	0x00, 0x04, // PPS length
	0x68, 0xce, 0x3c, 0x80,
}

func writeSynthVideo(mw *mp4.Writer) error {
	entry := &mp4.VisualSampleEntry{
		SampleEntry: mp4.SampleEntry{
			AnyTypeBox:         mp4.AnyTypeBox{Type: mp4.BoxTypeAvc1()},
			DataReferenceIndex: 1,
		},
		Width:  defaultWidth,
		Height: defaultHeight,
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeAvc1()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(mw, entry, mp4.Context{}); err != nil {
		return err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeAvcC()}); err != nil {
		return err
	}
	if _, err := mw.Write(avcC); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil {
		return err
	}
	_, err := mw.EndBox()
	return err
}

func writeSynthAudio(mw *mp4.Writer) error {
	entry := &mp4.AudioSampleEntry{
		SampleEntry: mp4.SampleEntry{
			AnyTypeBox:         mp4.AnyTypeBox{Type: mp4.BoxTypeMp4a()},
			DataReferenceIndex: 1,
		},
		ChannelCount: 2,
		SampleSize:   16,
		SampleRate:   48000 << 16,
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMp4a()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(mw, entry, mp4.Context{}); err != nil {
		return err
	}
	_, err := mw.EndBox()
	return err
}
