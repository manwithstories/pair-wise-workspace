// MP4 box writers for generated fixtures: movie/track headers, edit lists,
// handler and sample-description boxes, plus the sample tables.
package fixture

import (
	"fmt"
	"os"

	mp4 "github.com/abema/go-mp4"
)

func writeMvhd(w *mp4.Writer, c Clip) error {
	dur := uint64(c.Samples) * uint64(c.Delta)
	return writeBox(w, &mp4.Mvhd{
		Timescale:   1000,
		DurationV0:  uint32(dur / uint64(c.Timescale) * 1000),
		Rate:        0x00010000,
		Volume:      0x0100,
		Matrix:      identityMatrix(),
		NextTrackID: c.TrackID + 1,
	})
}

func identityMatrix() [9]int32 {
	return [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
}

func writeTkhd(w *mp4.Writer, c Clip) error {
	return writeBox(w, &mp4.Tkhd{
		TrackID:    c.TrackID,
		DurationV0: uint32(uint64(c.Samples) * uint64(c.Delta) / uint64(c.Timescale)),
		Volume:     boolToVol(c.Kind),
		Matrix:     identityMatrix(),
	})
}

func boolToVol(kind string) int16 {
	if kind == "audio" {
		return 0x0100
	}
	return 0
}

func writeEdts(w *mp4.Writer, c Clip) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeEdts()}); err != nil {
		return err
	}
	elst := &mp4.Elst{EntryCount: 1, Entries: []mp4.ElstEntry{{
		SegmentDurationV0: uint32(uint64(c.Samples) * uint64(c.Delta) / uint64(c.Timescale)),
		MediaTimeV0:       int32(c.Origin),
		MediaRateInteger:  1,
	}}}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeElst()}); err != nil {
		return err
	}
	elst.SetVersion(0)
	if _, err := mp4.Marshal(w, elst, mp4.Context{}); err != nil {
		return err
	}
	if _, err := w.EndBox(); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}

func writeMdhd(w *mp4.Writer, c Clip) error {
	return writeBox(w, &mp4.Mdhd{
		Timescale:  c.Timescale,
		DurationV0: uint32(uint64(c.Samples) * uint64(c.Delta)),
		Language:   [3]byte{'u', 'n', 'd'},
	})
}

func writeHdlr(w *mp4.Writer, c Clip) error {
	handler := [4]byte{'s', 'o', 'u', 'n'}
	name := "Archive Sound Handler"
	if c.Kind != "audio" {
		handler = [4]byte{'v', 'i', 'd', 'e'}
		name = "Archive Video Handler"
	}
	return writeBox(w, &mp4.Hdlr{HandlerType: handler, Name: name})
}

func writeStsd(w *mp4.Writer, c Clip) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsd()}); err != nil {
		return err
	}
	stsd := &mp4.Stsd{EntryCount: 1}
	if _, err := mp4.Marshal(w, stsd, mp4.Context{}); err != nil {
		return err
	}
	if c.Kind == "audio" {
		entry := &mp4.AudioSampleEntry{
			SampleEntry:  mp4.SampleEntry{AnyTypeBox: mp4.AnyTypeBox{Type: mp4.BoxTypeMp4a()}, DataReferenceIndex: 1},
			ChannelCount: 2,
			SampleSize:   16,
			SampleRate:   c.Timescale << 16,
		}
		if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMp4a()}); err != nil {
			return err
		}
		if _, err := mp4.Marshal(w, entry, mp4.Context{}); err != nil {
			return err
		}
		if _, err := w.EndBox(); err != nil {
			return err
		}
	} else {
		entry := &mp4.VisualSampleEntry{
			SampleEntry:     mp4.SampleEntry{AnyTypeBox: mp4.AnyTypeBox{Type: mp4.BoxTypeAvc1()}, DataReferenceIndex: 1},
			Width:           1920,
			Height:          1080,
			Horizresolution: 0x00480000,
			Vertresolution:  0x00480000,
			FrameCount:      1,
			Depth:           0x18,
			PreDefined3:     -1,
		}
		if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeAvc1()}); err != nil {
			return err
		}
		if _, err := mp4.Marshal(w, entry, mp4.Context{}); err != nil {
			return err
		}
		if _, err := w.EndBox(); err != nil {
			return err
		}
	}
	_, err := w.EndBox()
	return err
}

func writeStts(w *mp4.Writer, c Clip) error {
	return writeBox(w, &mp4.Stts{EntryCount: 1, Entries: []mp4.SttsEntry{
		{SampleCount: uint32(c.Samples), SampleDelta: c.Delta},
	}})
}

func writeStss(w *mp4.Writer, c Clip) error {
	if c.Kind == "audio" {
		return nil // audio tracks are all sync samples; stss omitted
	}
	entries := make([]uint32, 0, c.Samples/c.KeyEvery+1)
	for i := 0; i < c.Samples; i++ {
		if i%c.KeyEvery == 0 {
			entries = append(entries, uint32(i+1))
		}
	}
	return writeBox(w, &mp4.Stss{EntryCount: uint32(len(entries)), SampleNumber: entries})
}

func writeStsz(w *mp4.Writer, c Clip, sizes []uint32) error {
	if c.ConstSize {
		return writeBox(w, &mp4.Stsz{SampleSize: sizes[0], SampleCount: uint32(c.Samples)})
	}
	return writeBox(w, &mp4.Stsz{SampleSize: 0, SampleCount: uint32(c.Samples), EntrySize: sizes})
}

// Truncate removes the final n bytes of a generated MP4, simulating a
// transfer that lost the tail of the mdat payload.
func Truncate(path string, n int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if n <= 0 || n >= info.Size() {
		return fmt.Errorf("fixture: invalid truncation %d", n)
	}
	return os.Truncate(path, info.Size()-n)
}

var _ = mp4.StrToBoxType
