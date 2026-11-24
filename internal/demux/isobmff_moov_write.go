package demux

import (
	"github.com/abema/go-mp4"

	"relay/internal/media"
)

func writeMoov(mw *mp4.Writer, hdr FSEQHeader) error {
	timescale := uint32(1_000_000)
	matrix := [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}
	start := func(t mp4.BoxType) error { _, err := mw.StartBox(&mp4.BoxInfo{Type: t}); return err }
	end := mw.EndBox

	if err := start(mp4.BoxTypeMoov()); err != nil {
		return err
	}
	if err := start(mp4.BoxTypeMvhd()); err != nil {
		return err
	}
	if _, err := mp4.Marshal(mw, &mp4.Mvhd{
		FullBox: mp4.FullBox{Version: 1}, Timescale: timescale,
		Rate: 0x10000, Volume: 0x100, Matrix: matrix, NextTrackID: 2,
	}, mp4.Context{}); err != nil {
		return err
	}
	if _, err := end(); err != nil {
		return err
	}
	if err := start(mp4.BoxTypeTrak()); err != nil {
		return err
	}
	if err := start(mp4.BoxTypeTkhd()); err != nil {
		return err
	}
	tkhd := &mp4.Tkhd{
		FullBox: mp4.FullBox{Version: 1, Flags: [3]byte{0x00, 0x00, 0x07}},
		TrackID: hdr.SourceID, Matrix: matrix,
		Width:  hdr.Width << 16,
		Height: hdr.Height << 16,
	}
	if hdr.Kind == media.KindAudio {
		tkhd.Volume = 0x100
	}
	if _, err := mp4.Marshal(mw, tkhd, mp4.Context{}); err != nil {
		return err
	}
	if _, err := end(); err != nil {
		return err
	}
	if err := start(mp4.BoxTypeMdia()); err != nil {
		return err
	}
	if err := start(mp4.BoxTypeMdhd()); err != nil {
		return err
	}
	if _, err := mp4.Marshal(mw, &mp4.Mdhd{
		FullBox: mp4.FullBox{Version: 1}, Timescale: timescale,
		Language: [3]byte{'u', 'n', 'd'},
	}, mp4.Context{}); err != nil {
		return err
	}
	if _, err := end(); err != nil {
		return err
	}
	if err := start(mp4.BoxTypeHdlr()); err != nil {
		return err
	}
	handler := "vide"
	if hdr.Kind == media.KindAudio {
		handler = "soun"
	}
	if _, err := mp4.Marshal(mw, &mp4.Hdlr{
		HandlerType: brand(handler), Name: "relay",
	}, mp4.Context{}); err != nil {
		return err
	}
	if _, err := end(); err != nil {
		return err
	}
	if err := start(mp4.BoxTypeMinf()); err != nil {
		return err
	}
	if err := start(mp4.BoxTypeStbl()); err != nil {
		return err
	}
	if err := start(mp4.BoxTypeStsd()); err != nil {
		return err
	}
	if _, err := mp4.Marshal(mw, &mp4.Stsd{EntryCount: 0}, mp4.Context{}); err != nil {
		return err
	}
	if _, err := end(); err != nil { // stsd
		return err
	}
	if _, err := end(); err != nil { // stbl
		return err
	}
	if _, err := end(); err != nil { // minf
		return err
	}
	if _, err := end(); err != nil { // mdia
		return err
	}
	if err := start(mp4.BoxTypeMvex()); err != nil {
		return err
	}
	if err := start(mp4.BoxTypeTrex()); err != nil {
		return err
	}
	if _, err := mp4.Marshal(mw, &mp4.Trex{
		TrackID:                       hdr.SourceID,
		DefaultSampleDescriptionIndex: 1,
		DefaultSampleDuration:         1,
		DefaultSampleFlags:            0x2000000,
	}, mp4.Context{}); err != nil {
		return err
	}
	if _, err := end(); err != nil { // trex
		return err
	}
	if _, err := end(); err != nil { // mvex
		return err
	}
	if _, err := end(); err != nil { // trak
		return err
	}
	if _, err := end(); err != nil { // moov
		return err
	}
	return nil
}

// writeMoofMdat writes one fragment containing exactly one sample. The trun
