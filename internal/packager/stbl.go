package packager

import (
	mp4 "github.com/abema/go-mp4"
)

func init() {
	// Register the private PCM16LE sample-entry fourCC so go-mp4 can
	// marshal/parse it on the standard audio sample entry layout.
	mp4.AddAnyTypeBoxDef(&mp4.AudioSampleEntry{}, mp4.StrToBoxType(pcmCodec))
}

// writeMinf writes media information: a null dinf/dref and an stbl whose stsd
// carries either an avc1/avcC video entry or the private pcml audio entry.
func writeMinf(mw *mp4.Writer, tc TrackConfig) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMinf()}); err != nil {
		return err
	}
	// dinf/dref with a self-contained url entry.
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeDinf()}); err != nil {
		return err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeDref()}); err != nil {
		return err
	}
	dref := &mp4.Dref{FullBox: mp4.FullBox{}, EntryCount: 1}
	if _, err := mp4.Marshal(mw, dref, mp4.Context{}); err != nil {
		return err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeUrl()}); err != nil {
		return err
	}
	url := &mp4.Url{FullBox: mp4.FullBox{Flags: flags3(1)}}
	if _, err := mp4.Marshal(mw, url, mp4.Context{}); err != nil {
		return err
	}
	if err := endBox(mw); err != nil { // url
		return err
	}
	if err := endBox(mw); err != nil { // dref
		return err
	}
	if err := endBox(mw); err != nil { // dinf
		return err
	}

	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStbl()}); err != nil {
		return err
	}
	if err := writeStsd(mw, tc); err != nil {
		return err
	}
	if err := writeEmptyTable(mw, mp4.BoxTypeStts()); err != nil {
		return err
	}
	if err := writeEmptyStsc(mw); err != nil {
		return err
	}
	if err := writeEmptyStsz(mw); err != nil {
		return err
	}
	if err := writeEmptyTable(mw, mp4.BoxTypeStco()); err != nil {
		return err
	}
	return endBox(mw) // stbl
}

// writeEmptyTable emits version/flags (4) plus a zero entry_count (4).
func writeEmptyTable(mw *mp4.Writer, t mp4.BoxType) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: t}); err != nil {
		return err
	}
	if _, err := mw.Write([]byte{
		0, 0, 0, 0, // version + flags
		0, 0, 0, 0, // entry_count = 0
	}); err != nil {
		return err
	}
	return endBox(mw)
}

// writeEmptyStsc writes version/flags + zero entries. stsc entries are 12
// bytes each; zero entries means the fragment trun drives sample grouping.
func writeEmptyStsc(mw *mp4.Writer) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsc()}); err != nil {
		return err
	}
	if _, err := mw.Write([]byte{
		0, 0, 0, 0, // version + flags
		0, 0, 0, 0, // entry_count = 0
	}); err != nil {
		return err
	}
	return endBox(mw)
}

// writeEmptyStsz writes version/flags + sample_size=0 (sizes are in trun)
// plus sample_count=0.
func writeEmptyStsz(mw *mp4.Writer) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsz()}); err != nil {
		return err
	}
	if _, err := mw.Write([]byte{
		0, 0, 0, 0, // version + flags
		0, 0, 0, 0, // sample_size = 0 (variable; per-sample via trun)
		0, 0, 0, 0, // sample_count = 0
	}); err != nil {
		return err
	}
	return endBox(mw)
}

func writeStsd(mw *mp4.Writer, tc TrackConfig) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsd()}); err != nil {
		return err
	}
	if _, err := mw.Write([]byte{0, 0, 0, 0, 0, 0, 0, 1}); err != nil {
		return err // version/flags + entry_count=1
	}
	if tc.Video != nil {
		if err := writeAVC1(mw, *tc.Video); err != nil {
			return err
		}
	} else if tc.Audio != nil {
		if err := writePCML(mw, *tc.Audio); err != nil {
			return err
		}
	}
	return endBox(mw) // stsd
}

// writeAVC1 writes a visual sample entry with an avcC child built from the
// raw SPS/PPS NAL units (start codes stripped).
func writeAVC1(mw *mp4.Writer, vc VideoConfig) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.StrToBoxType("avc1")}); err != nil {
		return err
	}
	payload := make([]byte, 78) // fixed visual sample-entry size
	u16(payload[24:], vc.Width)
	u16(payload[26:], vc.Height)
	u32(payload[28:], 0x00480000) // horizresolution 72 dpi
	u32(payload[32:], 0x00480000) // vertresolution
	u16(payload[40:], 1)          // frame_count
	// compressorname (32 bytes) at offset 46, depth/pre_defined at the tail
	payload[46] = 12
	copy(payload[47:], "streamcut-h264")
	u16(payload[74:], 0x0018) // depth = 24
	payload[76] = 0x11        // pre_defined = -1
	if _, err := mw.Write(payload); err != nil {
		return err
	}
	if err := writeAvcC(mw, vc.SPS, vc.PPS); err != nil {
		return err
	}
	return endBox(mw) // avc1
}

func writeAvcC(mw *mp4.Writer, sps, pps []byte) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.StrToBoxType("avcC")}); err != nil {
		return err
	}
	body := []byte{
		0x01,             // configurationVersion
		sps[1], sps[2], sps[3], // profile/level from the SPS NAL
		0xFF,             // lengthSizeMinusOne = 3 (4-byte length)
		0xE1,             // one SPS
	}
	body = appendU16(body, uint16(len(sps)))
	body = append(body, sps...)
	body = append(body, 0x01) // one PPS
	body = appendU16(body, uint16(len(pps)))
	body = append(body, pps...)
	if _, err := mw.Write(body); err != nil {
		return err
	}
	return endBox(mw)
}

// writePCML writes the private PCM16LE audio sample entry as a registered
// custom box type (pcml) built on the standard AudioSampleEntry layout, so
// the track is never mislabeled as a codec we claim to decode.
func writePCML(mw *mp4.Writer, ac AudioConfig) error {
	entry := &mp4.AudioSampleEntry{
		SampleEntry: mp4.SampleEntry{
			AnyTypeBox:       mp4.AnyTypeBox{Type: mp4.StrToBoxType(pcmCodec)},
			DataReferenceIndex: 1,
		},
		ChannelCount: ac.Channels,
		SampleSize:   16,
		SampleRate:   ac.SampleRate << 16, // fixed-point 16.16
	}
	return marshalBox(mw, mp4.StrToBoxType(pcmCodec), entry)
}

func appendU16(b []byte, v uint16) []byte {
	return append(b, byte(v>>8), byte(v))
}
