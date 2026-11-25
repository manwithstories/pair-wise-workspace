package mux

import (
	"encoding/binary"

	"github.com/abema/go-mp4"
)

func startBox(w *mp4.Writer, t mp4.BoxType) (*mp4.BoxInfo, error) {
	return w.StartBox(&mp4.BoxInfo{Type: t})
}

func endBox(w *mp4.Writer) error {
	_, err := w.EndBox()
	return err
}

func marshalLeaf(w *mp4.Writer, b mp4.IImmutableBox) error {
	if _, err := startBox(w, b.GetType()); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, b, mp4.Context{}); err != nil {
		return err
	}
	return endBox(w)
}

func writeFileType(w *mp4.Writer) error {
	if _, err := startBox(w, mp4.BoxTypeFtyp()); err != nil {
		return err
	}
	ftyp := &mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 0x200,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
			{CompatibleBrand: [4]byte{'i', 's', 'o', '2'}},
			{CompatibleBrand: [4]byte{'m', 'p', '4', '1'}},
		},
	}
	if _, err := mp4.Marshal(w, ftyp, mp4.Context{}); err != nil {
		return err
	}
	return endBox(w)
}

func writeMovieHeader(w *mp4.Writer, duration uint32) error {
	return marshalLeaf(w, &mp4.Mvhd{
		Timescale:   uint32(Timescale),
		DurationV0:  duration,
		Rate:        0x00010000,
		Volume:      0x0100,
		Matrix:      [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000},
		NextTrackID: 3,
	})
}

func writeMediaHeader(w *mp4.Writer, duration uint32) error {
	return marshalLeaf(w, &mp4.Mdhd{Timescale: uint32(Timescale), DurationV0: duration})
}

func writeHandler(w *mp4.Writer, handler [4]byte, name string) error {
	return marshalLeaf(w, &mp4.Hdlr{HandlerType: handler, Name: name})
}

func writeDinf(w *mp4.Writer) error {
	if _, err := startBox(w, mp4.BoxTypeDinf()); err != nil {
		return err
	}
	if _, err := startBox(w, mp4.BoxTypeDref()); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, &mp4.Dref{EntryCount: 1}, mp4.Context{}); err != nil {
		return err
	}
	if _, err := startBox(w, mp4.BoxTypeUrl()); err != nil {
		return err
	}
	if _, err := w.Write([]byte{0x00, 0x00, 0x00, 0x01}); err != nil { // self-contained flag
		return err
	}
	if err := endBox(w); err != nil { // url
		return err
	}
	if err := endBox(w); err != nil { // dref
		return err
	}
	return endBox(w) // dinf
}

func writeVideoSampleDescription(w *mp4.Writer, width, height uint16) error {
	if _, err := startBox(w, mp4.BoxTypeStsd()); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, &mp4.Stsd{EntryCount: 1}, mp4.Context{}); err != nil {
		return err
	}
	if _, err := startBox(w, mp4.BoxTypeAvc1()); err != nil {
		return err
	}
	payload := make([]byte, 78)
	binary.BigEndian.PutUint16(payload[24:26], width)
	binary.BigEndian.PutUint16(payload[26:28], height)
	binary.BigEndian.PutUint32(payload[28:32], 0x00480000) // 72 dpi
	binary.BigEndian.PutUint32(payload[32:36], 0x00480000)
	binary.BigEndian.PutUint16(payload[40:42], 1) // frame count
	binary.BigEndian.PutUint16(payload[74:76], 24)
	binary.BigEndian.PutUint16(payload[76:78], 0xffff)
	if _, err := w.Write(payload); err != nil {
		return err
	}
	if err := endBox(w); err != nil { // avc1
		return err
	}
	return endBox(w) // stsd
}

func writeAudioSampleDescription(w *mp4.Writer, channels uint16) error {
	if _, err := startBox(w, mp4.BoxTypeStsd()); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, &mp4.Stsd{EntryCount: 1}, mp4.Context{}); err != nil {
		return err
	}
	if _, err := startBox(w, mp4.BoxTypeMp4a()); err != nil {
		return err
	}
	payload := make([]byte, 28)
	binary.BigEndian.PutUint16(payload[8:10], channels)
	binary.BigEndian.PutUint16(payload[10:12], 16)
	binary.BigEndian.PutUint16(payload[16:18], 0)
	binary.BigEndian.PutUint32(payload[20:24], uint32(TargetAudioRate)<<16)
	if _, err := w.Write(payload); err != nil {
		return err
	}
	if err := endBox(w); err != nil { // mp4a
		return err
	}
	return endBox(w) // stsd
}

func writeEmptySampleTables(w *mp4.Writer, video bool, width uint16, height uint16, channels uint16) error {
	if _, err := startBox(w, mp4.BoxTypeStbl()); err != nil {
		return err
	}
	if video {
		if err := writeVideoSampleDescription(w, width, height); err != nil {
			return err
		}
	} else {
		if err := writeAudioSampleDescription(w, channels); err != nil {
			return err
		}
	}
	if err := marshalLeaf(w, &mp4.Stts{}); err != nil {
		return err
	}
	if err := marshalLeaf(w, &mp4.Stsc{}); err != nil {
		return err
	}
	if err := marshalLeaf(w, &mp4.Stsz{}); err != nil {
		return err
	}
	return endBox(w) // stbl
}

func writeTrack(w *mp4.Writer, trackID uint32, handler [4]byte, name string,
	duration uint32, video bool, width, height uint16, channels uint16) error {
	if _, err := startBox(w, mp4.BoxTypeTrak()); err != nil {
		return err
	}
	if err := marshalLeaf(w, &mp4.Tkhd{
		TrackID: trackID, DurationV0: duration, Volume: tkhdVolume(video),
		Width: uint32(width) << 16, Height: uint32(height) << 16,
		Matrix: [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000},
	}); err != nil {
		return err
	}
	if _, err := startBox(w, mp4.BoxTypeMdia()); err != nil {
		return err
	}
	if err := writeMediaHeader(w, duration); err != nil {
		return err
	}
	if err := writeHandler(w, handler, name); err != nil {
		return err
	}
	if _, err := startBox(w, mp4.BoxTypeMinf()); err != nil {
		return err
	}
	if video {
		if err := marshalLeaf(w, &mp4.Vmhd{}); err != nil {
			return err
		}
	} else if err := marshalLeaf(w, &mp4.Smhd{}); err != nil {
		return err
	}
	if err := writeDinf(w); err != nil {
		return err
	}
	if err := writeEmptySampleTables(w, video, width, height, channels); err != nil {
		return err
	}
	if err := endBox(w); err != nil { // minf
		return err
	}
	if err := endBox(w); err != nil { // mdia
		return err
	}
	return endBox(w) // trak
}

func tkhdVolume(video bool) int16 {
	if video {
		return 0
	}
	return 0x0100
}

func writeTrex(w *mp4.Writer, trackID uint32) error {
	return marshalLeaf(w, &mp4.Trex{
		TrackID:                       trackID,
		DefaultSampleDescriptionIndex: 1,
	})
}
