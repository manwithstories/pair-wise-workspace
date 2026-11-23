package main

import (
	"github.com/abema/go-mp4"

	"archive/internal/manifest"
	"archive/internal/media"
)

// addBox writes one leaf box: StartBox + Marshal + EndBox.
func addBox(w *mp4.Writer, typ mp4.BoxType, payload mp4.IImmutableBox) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: typ}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, payload, mp4.Context{}); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}

// writeMoov appends the moov box with one trak per manifest track. Only
// the boxes the archive pipeline reads are written: mdhd (timescale),
// elst (edit list), stts/stss/stsz/stco (sample tables).
func writeMoov(w *mp4.Writer, m *manifest.Manifest, offsets [][]uint64) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		return media.New(media.KindInternal, "fixture.mp4", "", -1, "start moov", err)
	}
	mvhd := &mp4.Mvhd{Timescale: m.MovieTimescale, NextTrackID: 3}
	if err := addBox(w, mp4.BoxTypeMvhd(), mvhd); err != nil {
		return media.New(media.KindInternal, "fixture.mp4", "", -1, "mvhd", err)
	}
	for ti, tr := range m.Tracks {
		if err := writeTrak(w, tr, offsets[ti]); err != nil {
			return err
		}
	}
	if _, err := w.EndBox(); err != nil {
		return media.New(media.KindInternal, "fixture.mp4", "", -1, "end moov", err)
	}
	return nil
}

func writeTrak(w *mp4.Writer, tr manifest.RawTrack, offs []uint64) error {
	op := func(stage string, err error) error {
		if err != nil {
			return media.New(media.KindInternal, "fixture.mp4", tr.ID, -1, stage, err)
		}
		return nil
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()}); err != nil {
		return op("start trak", err)
	}
	// edts/elst
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeEdts()}); err != nil {
		return op("start edts", err)
	}
	elst := &mp4.Elst{}
	for _, e := range tr.EditList {
		elst.Entries = append(elst.Entries, mp4.ElstEntry{
			SegmentDurationV1: uint64(e.SegmentDuration),
			MediaTimeV1:       e.MediaTime,
			MediaRateInteger:  1,
		})
	}
	elst.EntryCount = uint32(len(elst.Entries))
	elst.SetVersion(1)
	if err := addBox(w, mp4.BoxTypeElst(), elst); err != nil {
		return op("elst", err)
	}
	if _, err := w.EndBox(); err != nil { // edts
		return op("end edts", err)
	}
	// mdia
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdia()}); err != nil {
		return op("start mdia", err)
	}
	var dur uint32
	for _, s := range tr.Samples {
		dur += uint32(s.Duration)
	}
	mdhd := &mp4.Mdhd{Timescale: tr.Timescale, DurationV0: dur}
	if err := addBox(w, mp4.BoxTypeMdhd(), mdhd); err != nil {
		return op("mdhd", err)
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMinf()}); err != nil {
		return op("start minf", err)
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStbl()}); err != nil {
		return op("start stbl", err)
	}
	if err := writeSampleTables(w, tr, offs); err != nil {
		return err
	}
	for i := 0; i < 3; i++ { // stbl, minf, mdia, trak closed below
		if _, err := w.EndBox(); err != nil {
			return op("end box", err)
		}
	}
	_, err := w.EndBox() // trak
	return op("end trak", err)
}

// writeSampleTables writes stts/stss/stsz/stco. One chunk per track keeps
// stco compact; per-sample offsets are chunk offset + prefix sums.
func writeSampleTables(w *mp4.Writer, tr manifest.RawTrack, offs []uint64) error {
	op := func(stage string, err error) error {
		if err != nil {
			return media.New(media.KindInternal, "fixture.mp4", tr.ID, -1, stage, err)
		}
		return nil
	}
	// stts: run-length over constant durations.
	var stts mp4.Stts
	for _, s := range tr.Samples {
		n := len(stts.Entries)
		if n > 0 && stts.Entries[n-1].SampleDelta == uint32(s.Duration) {
			stts.Entries[n-1].SampleCount++
		} else {
			stts.Entries = append(stts.Entries, mp4.SttsEntry{
				SampleCount: 1, SampleDelta: uint32(s.Duration)})
		}
	}
	stts.EntryCount = uint32(len(stts.Entries))
	if err := addBox(w, mp4.BoxTypeStts(), &stts); err != nil {
		return op("stts", err)
	}
	// stss: 1-based keyframe sample numbers.
	var stss mp4.Stss
	for i, s := range tr.Samples {
		if s.Keyframe {
			stss.SampleNumber = append(stss.SampleNumber, uint32(i+1))
		}
	}
	stss.EntryCount = uint32(len(stss.SampleNumber))
	if err := addBox(w, mp4.BoxTypeStss(), &stss); err != nil {
		return op("stss", err)
	}
	// stsz
	stsz := mp4.Stsz{SampleCount: uint32(len(tr.Samples))}
	for _, s := range tr.Samples {
		stsz.EntrySize = append(stsz.EntrySize, s.Size)
	}
	if err := addBox(w, mp4.BoxTypeStsz(), &stsz); err != nil {
		return op("stsz", err)
	}
	// stco: single chunk at the track's first sample.
	chunk := uint32(0)
	if len(offs) > 0 {
		chunk = uint32(offs[0])
	}
	stco := mp4.Stco{EntryCount: 1, ChunkOffset: []uint32{chunk}}
	if err := addBox(w, mp4.BoxTypeStco(), &stco); err != nil {
		return op("stco", err)
	}
	return nil
}
