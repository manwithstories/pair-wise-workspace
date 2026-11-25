package packager

import (
	mp4 "github.com/abema/go-mp4"
)

// flags3 packs a 24-bit box flag value into the FullBox [3]byte layout.
func flags3(v uint32) [3]byte {
	return [3]byte{byte(v >> 16), byte(v >> 8), byte(v)}
}

// matrixIdentity is the standard 0x00010000/0x40000000 transformation.
var matrixIdentity = [9]int32{
	0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000,
}

func writeMfhd(mw *mp4.Writer, seq uint32) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMfhd()}); err != nil {
		return err
	}
	mfhd := &mp4.Mfhd{SequenceNumber: seq}
	if _, err := mp4.Marshal(mw, mfhd, mp4.Context{}); err != nil {
		return err
	}
	return endBox(mw)
}

// writeTraf writes one track fragment: tfhd + trun. dataOffset is the
// precomputed trun data_offset relative to the moof box start.
func writeTraf(mw *mp4.Writer, tid uint32, samples []Sample, dataOffset int64) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTraf()}); err != nil {
		return err
	}
	tfhd := &mp4.Tfhd{
		FullBox: mp4.FullBox{Flags: flags3(mp4.TfhdDefaultBaseIsMoof)},
		TrackID: tid,
	}
	if err := marshalBox(mw, mp4.BoxTypeTfhd(), tfhd); err != nil {
		return err
	}

	trun := &mp4.Trun{
		FullBox: mp4.FullBox{
			Flags: flags3(0x000100 | 0x000200 | 0x000001),
		},
		SampleCount: uint32(len(samples)),
		DataOffset:  int32(dataOffset),
	}
	for _, s := range samples {
		trun.Entries = append(trun.Entries, mp4.TrunEntry{
			SampleDuration: s.Duration,
			SampleSize:     uint32(len(s.Data)),
			SampleFlags:    sampleFlags(s.IsSync),
		})
	}
	if err := marshalBox(mw, mp4.BoxTypeTrun(), trun); err != nil {
		return err
	}
	return endBox(mw) // traf
}

func sampleFlags(sync bool) uint32 {
	if sync {
		return 0x02000000
	}
	return 0x01010000
}

// marshalBox opens a box, marshals the struct, and closes it in one step.
func marshalBox(mw *mp4.Writer, t mp4.BoxType, b mp4.IImmutableBox) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: t}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(mw, b, mp4.Context{}); err != nil {
		return err
	}
	return endBox(mw)
}

// writeTrak writes the full trak hierarchy for one configured track.
func writeTrak(mw *mp4.Writer, tc TrackConfig) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()}); err != nil {
		return err
	}
	if err := writeTkhd(mw, tc); err != nil {
		return err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdia()}); err != nil {
		return err
	}
	if err := writeMdhdHdlr(mw, tc); err != nil {
		return err
	}
	if err := writeMinf(mw, tc); err != nil {
		return err
	}
	if err := endBox(mw); err != nil { // mdia
		return err
	}
	if err := writeTrex(mw, tc.TrackID); err != nil {
		return err
	}
	return endBox(mw) // trak
}

func writeTkhd(mw *mp4.Writer, tc TrackConfig) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTkhd()}); err != nil {
		return err
	}
	tkhd := &mp4.Tkhd{
		FullBox: mp4.FullBox{Flags: flags3(0x000007)},
		TrackID: tc.TrackID,
		Matrix:  matrixIdentity,
	}
	if tc.Video != nil {
		tkhd.Width = uint32(tc.Video.Width) << 16
		tkhd.Height = uint32(tc.Video.Height) << 16
	} else {
		tkhd.Volume = 0x0100
	}
	if _, err := mp4.Marshal(mw, tkhd, mp4.Context{}); err != nil {
		return err
	}
	return endBox(mw)
}

func writeMdhdHdlr(mw *mp4.Writer, tc TrackConfig) error {
	mdhd := &mp4.Mdhd{Timescale: tc.Timescale}
	if err := marshalBox(mw, mp4.BoxTypeMdhd(), mdhd); err != nil {
		return err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeHdlr()}); err != nil {
		return err
	}
	hdlr := &mp4.Hdlr{}
	if tc.Video != nil {
		hdlr.HandlerType = [4]byte{'v', 'i', 'd', 'e'}
		hdlr.Name = "streamcut-video"
	} else {
		hdlr.HandlerType = [4]byte{'s', 'o', 'u', 'n'}
		hdlr.Name = "streamcut-pcm"
	}
	if _, err := mp4.Marshal(mw, hdlr, mp4.Context{}); err != nil {
		return err
	}
	return endBox(mw)
}

func writeTrex(mw *mp4.Writer, tid uint32) error {
	trex := &mp4.Trex{TrackID: tid, DefaultSampleDescriptionIndex: 1}
	return marshalBox(mw, mp4.BoxTypeTrex(), trex)
}

// endBox adapts the two-value Writer.EndBox to the package's error-only style.
func endBox(mw *mp4.Writer) error {
	_, err := mw.EndBox()
	return err
}
