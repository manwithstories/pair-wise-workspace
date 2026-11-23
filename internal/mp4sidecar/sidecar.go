// Package mp4sidecar emits an ISO-BMFF companion file that records each
// track's timescale, sample durations and keyframe positions. It is a
// manifest, not a muxed media file: payloads are stored verbatim in mdat so
// the sidecar can be re-read and compared against the sequence.
package mp4sidecar

import (
	"bytes"
	"fmt"
	"io"

	mp4 "github.com/abema/go-mp4"

	"medpipe/internal/clock"
	"medpipe/internal/model"
)

// Summary is what a reader extracts back from a written sidecar.
type Summary struct {
	Tracks       []TrackSummary
	PayloadBytes int
}

// TrackSummary mirrors the per-track facts used for cross-checking.
type TrackSummary struct {
	ID               string
	Timescale        uint32
	SampleCount      int
	KeyframeCount    int
	TotalTicks       uint64
	PayloadByteCount int
}

// Write serializes the timeline to w. Sample durations are the actual VFR
// intervals in the track timescale, so non-uniform frame rate survives.
func Write(w io.WriteSeeker, tl *clock.Timeline, frames map[string][]model.Frame) error {
	mw := mp4.NewWriter(w)
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()}); err != nil {
		return wrap("ftyp", err)
	}
	if _, err := mp4.Marshal(mw, &mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', '5'},
		MinorVersion: 0,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', '5'}},
			{CompatibleBrand: [4]byte{'m', 'p', '4', '1'}},
		},
	}, mp4.Context{}); err != nil {
		return wrap("ftyp", err)
	}
	if _, err := mw.EndBox(); err != nil {
		return wrap("ftyp", err)
	}

	// mdat comes before moov so the moov is written with final offsets known.
	var payload bytes.Buffer
	chunkOffsets := make(map[string][]uint32, len(tl.Tracks))
	base := int64(0) // absolute offset of mdat payload, patched below
	_ = base
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()}); err != nil {
		return wrap("mdat", err)
	}
	payloadStart, err := w.Seek(0, io.SeekCurrent)
	if err != nil {
		return wrap("mdat", err)
	}
	for i := range tl.Tracks {
		id := tl.Tracks[i].Track.ID
		for _, fr := range frames[id] {
			chunkOffsets[id] = append(chunkOffsets[id], uint32(int64(payloadStart)+int64(payload.Len())))
			payload.Write(fr.Payload)
		}
	}
	if _, err := mw.Write(payload.Bytes()); err != nil {
		return wrap("mdat", err)
	}
	if _, err := mw.EndBox(); err != nil {
		return wrap("mdat", err)
	}

	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		return wrap("moov", err)
	}
	// A movie header is mandatory before any trak; it is emitted as a full
	// box so its version/flags word is present.
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMvhd()}); err != nil {
		return wrap("mvhd", err)
	}
	if _, err := mp4.Marshal(mw, &mp4.Mvhd{
		Timescale:   1000,
		DurationV0:  uint32(uint64(tl.DurationSeconds()*1000) + 1),
		Rate:        0x00010000,
		Volume:      0x0100,
		Matrix:      identityMatrix(),
		NextTrackID: uint32(len(tl.Tracks) + 1),
	}, mp4.Context{}); err != nil {
		return wrap("mvhd", err)
	}
	if _, err := mw.EndBox(); err != nil {
		return wrap("mvhd", err)
	}
	for i := range tl.Tracks {
		if err := writeTrak(mw, &tl.Tracks[i], frames[tl.Tracks[i].Track.ID], chunkOffsets[tl.Tracks[i].Track.ID], uint32(i+1)); err != nil {
			return err
		}
	}
	if _, err := mw.EndBox(); err != nil {
		return wrap("moov", err)
	}
	return nil
}

func identityMatrix() [9]int32 {
	return [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
}

func writeTrak(mw *mp4.Writer, tt *clock.TrackTimeline, frames []model.Frame, offsets []uint32, trackID uint32) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()}); err != nil {
		return wrap("trak", err)
	}
	mediaTicks := scaleTicks(tt.EndNS-tt.StartNS, tt.Track.Timescale)
	width, height := uint32(0), uint32(0)
	if tt.Track.Kind == model.KindVideo {
		for _, fr := range frames {
			if fr.Width > 0 && fr.Height > 0 {
				width, height = uint32(fr.Width)<<16, uint32(fr.Height)<<16
				break
			}
		}
	}
	volume := int16(0)
	if tt.Track.Kind == model.KindAudio {
		volume = 0x0100
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTkhd()}); err != nil {
		return wrap("tkhd", err)
	}
	if _, err := mp4.Marshal(mw, &mp4.Tkhd{
		TrackID:    trackID,
		DurationV0: uint32(mediaTicks),
		Layer:      0,
		Volume:     volume,
		Matrix:     identityMatrix(),
		Width:      width,
		Height:     height,
	}, mp4.Context{}); err != nil {
		return wrap("tkhd", err)
	}
	if _, err := mw.EndBox(); err != nil {
		return wrap("tkhd", err)
	}

	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdia()}); err != nil {
		return wrap("mdia", err)
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdhd()}); err != nil {
		return wrap("mdhd", err)
	}
	if _, err := mp4.Marshal(mw, &mp4.Mdhd{
		Timescale:  tt.Track.Timescale,
		DurationV0: uint32(mediaTicks),
	}, mp4.Context{}); err != nil {
		return wrap("mdhd", err)
	}
	if _, err := mw.EndBox(); err != nil {
		return wrap("mdhd", err)
	}
	handler := [4]byte{'s', 'o', 'u', 'n'}
	if tt.Track.Kind == model.KindVideo {
		handler = [4]byte{'v', 'i', 'd', 'e'}
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeHdlr()}); err != nil {
		return wrap("hdlr", err)
	}
	if _, err := mp4.Marshal(mw, &mp4.Hdlr{HandlerType: handler, Name: tt.Track.ID}, mp4.Context{}); err != nil {
		return wrap("hdlr", err)
	}
	if _, err := mw.EndBox(); err != nil {
		return wrap("hdlr", err)
	}

	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMinf()}); err != nil {
		return wrap("minf", err)
	}
	if tt.Track.Kind == model.KindVideo {
		if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeVmhd()}); err != nil {
			return wrap("vmhd", err)
		}
		if _, err := mp4.Marshal(mw, &mp4.Vmhd{}, mp4.Context{}); err != nil {
			return wrap("vmhd", err)
		}
		if _, err := mw.EndBox(); err != nil {
			return wrap("vmhd", err)
		}
	} else {
		if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeSmhd()}); err != nil {
			return wrap("smhd", err)
		}
		if _, err := mp4.Marshal(mw, &mp4.Smhd{}, mp4.Context{}); err != nil {
			return wrap("smhd", err)
		}
		if _, err := mw.EndBox(); err != nil {
			return wrap("smhd", err)
		}
	}
	if err := writeDinf(mw); err != nil {
		return err
	}
	if err := writeStbl(mw, tt, frames, offsets); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil {
		return wrap("minf", err)
	}
	if _, err := mw.EndBox(); err != nil {
		return wrap("mdia", err)
	}
	if _, err := mw.EndBox(); err != nil {
		return wrap("trak", err)
	}
	return nil
}

func writeDinf(mw *mp4.Writer) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeDinf()}); err != nil {
		return wrap("dinf", err)
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeDref()}); err != nil {
		return wrap("dref", err)
	}
	if _, err := mp4.Marshal(mw, &mp4.Dref{EntryCount: 1}, mp4.Context{}); err != nil {
		return wrap("dref", err)
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeUrl()}); err != nil {
		return wrap("url", err)
	}
	url := &mp4.Url{}
	url.SetFlags(1) // self-contained
	if _, err := mp4.Marshal(mw, url, mp4.Context{}); err != nil {
		return wrap("url", err)
	}
	if _, err := mw.EndBox(); err != nil {
		return wrap("url", err)
	}
	if _, err := mw.EndBox(); err != nil {
		return wrap("dref", err)
	}
	if _, err := mw.EndBox(); err != nil {
		return wrap("dinf", err)
	}
	return nil
}

func writeStbl(mw *mp4.Writer, tt *clock.TrackTimeline, frames []model.Frame, offsets []uint32) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStbl()}); err != nil {
		return wrap("stbl", err)
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsd()}); err != nil {
		return wrap("stsd", err)
	}
	if _, err := mp4.Marshal(mw, &mp4.Stsd{EntryCount: 0}, mp4.Context{}); err != nil {
		return wrap("stsd", err)
	}
	if _, err := mw.EndBox(); err != nil {
		return wrap("stsd", err)
	}

	entries := buildStts(tt)
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStts()}); err != nil {
		return wrap("stts", err)
	}
	if _, err := mp4.Marshal(mw, &mp4.Stts{EntryCount: uint32(len(entries)), Entries: entries}, mp4.Context{}); err != nil {
		return wrap("stts", err)
	}
	if _, err := mw.EndBox(); err != nil {
		return wrap("stts", err)
	}

	var keyframes []uint32
	for i, fr := range frames {
		if fr.Keyframe {
			keyframes = append(keyframes, uint32(i+1))
		}
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStss()}); err != nil {
		return wrap("stss", err)
	}
	if _, err := mp4.Marshal(mw, &mp4.Stss{EntryCount: uint32(len(keyframes)), SampleNumber: keyframes}, mp4.Context{}); err != nil {
		return wrap("stss", err)
	}
	if _, err := mw.EndBox(); err != nil {
		return wrap("stss", err)
	}

	sizes := make([]uint32, len(frames))
	for i, fr := range frames {
		sizes[i] = uint32(len(fr.Payload))
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsz()}); err != nil {
		return wrap("stsz", err)
	}
	if _, err := mp4.Marshal(mw, &mp4.Stsz{SampleCount: uint32(len(frames)), EntrySize: sizes}, mp4.Context{}); err != nil {
		return wrap("stsz", err)
	}
	if _, err := mw.EndBox(); err != nil {
		return wrap("stsz", err)
	}

	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsc()}); err != nil {
		return wrap("stsc", err)
	}
	if _, err := mp4.Marshal(mw, &mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: 1, SampleDescriptionIndex: 1}}}, mp4.Context{}); err != nil {
		return wrap("stsc", err)
	}
	if _, err := mw.EndBox(); err != nil {
		return wrap("stsc", err)
	}

	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStco()}); err != nil {
		return wrap("stco", err)
	}
	if _, err := mp4.Marshal(mw, &mp4.Stco{EntryCount: uint32(len(offsets)), ChunkOffset: offsets}, mp4.Context{}); err != nil {
		return wrap("stco", err)
	}
	if _, err := mw.EndBox(); err != nil {
		return wrap("stco", err)
	}

	if _, err := mw.EndBox(); err != nil {
		return wrap("stbl", err)
	}
	return nil
}

// buildStts collapses equal consecutive sample durations into run-length
// entries, which is what a VFR track looks like in a real file.
func buildStts(tt *clock.TrackTimeline) []mp4.SttsEntry {
	if len(tt.Frames) == 0 {
		return nil
	}
	var out []mp4.SttsEntry
	for i := range tt.Frames {
		dur := tt.Frames[i].IntervalNS
		if dur <= 0 {
			dur = 1
		}
		ticks := uint32(scaleTicks(dur, tt.Track.Timescale))
		if ticks == 0 {
			ticks = 1
		}
		if n := len(out); n > 0 && out[n-1].SampleDelta == ticks {
			out[n-1].SampleCount++
			continue
		}
		out = append(out, mp4.SttsEntry{SampleCount: 1, SampleDelta: ticks})
	}
	return out
}

func scaleTicks(ns int64, timescale uint32) uint64 {
	if ns <= 0 || timescale == 0 {
		return 0
	}
	return uint64(ns) * uint64(timescale) / 1e9
}

// Read parses a sidecar produced by Write and returns its summary. Only the
// boxes this package emits are required to be present.
func Read(r io.ReadSeeker) (*Summary, error) {
	base := []mp4.BoxType{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia()}
	find := func(tail ...mp4.BoxType) ([]*mp4.BoxInfoWithPayload, error) {
		path := append(append([]mp4.BoxType{}, base...), tail...)
		boxes, err := mp4.ExtractBoxWithPayload(r, nil, path)
		if err != nil {
			return nil, err
		}
		return boxes, nil
	}
	mdhds, err := find(mp4.BoxTypeMdhd())
	if err != nil {
		return nil, wrap("read mdhd", err)
	}
	hdlrs, err := find(mp4.BoxTypeHdlr())
	if err != nil {
		return nil, wrap("read hdlr", err)
	}
	stblTail := func(tail mp4.BoxType) ([]*mp4.BoxInfoWithPayload, error) {
		return find(mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), tail)
	}
	sttsBoxes, err := stblTail(mp4.BoxTypeStts())
	if err != nil {
		return nil, wrap("read stts", err)
	}
	sizes, err := stblTail(mp4.BoxTypeStsz())
	if err != nil {
		return nil, wrap("read stsz", err)
	}
	stssBoxes, err := stblTail(mp4.BoxTypeStss())
	if err != nil {
		return nil, wrap("read stss", err)
	}
	if len(hdlrs) != len(sttsBoxes) || len(hdlrs) != len(sizes) || len(hdlrs) != len(stssBoxes) || len(hdlrs) != len(mdhds) {
		return nil, model.New(model.KindCorrupt, "mp4sidecar.Read", "track boxes are inconsistent")
	}
	sum := &Summary{}
	for i := range hdlrs {
		mdhdp := mdhds[i].Payload.(*mp4.Mdhd)
		hdlrp := hdlrs[i].Payload.(*mp4.Hdlr)
		sttsp := sttsBoxes[i].Payload.(*mp4.Stts)
		stszp := sizes[i].Payload.(*mp4.Stsz)
		stssp := stssBoxes[i].Payload.(*mp4.Stss)
		var total uint64
		var count int
		for _, e := range sttsp.Entries {
			total += uint64(e.SampleCount) * uint64(e.SampleDelta)
			count += int(e.SampleCount)
		}
		ts := TrackSummary{
			ID:            hdlrp.Name,
			Timescale:     mdhdp.Timescale,
			SampleCount:   count,
			KeyframeCount: len(stssp.SampleNumber),
			TotalTicks:    total,
		}
		for _, s := range stszp.EntrySize {
			ts.PayloadByteCount += int(s)
			sum.PayloadBytes += int(s)
		}
		if count != int(stszp.SampleCount) {
			return nil, model.New(model.KindCorrupt, "mp4sidecar.Read",
				fmt.Sprintf("stts sample count %d does not match stsz %d", count, stszp.SampleCount))
		}
		sum.Tracks = append(sum.Tracks, ts)
	}
	return sum, nil
}

func wrap(op string, err error) error {
	return model.Wrap(model.KindInvariant, "mp4sidecar."+op, err)
}
