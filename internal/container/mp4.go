// Package container writes self-generated media samples into a minimal valid
// MP4 file with go-mp4 and parses the result back for round-trip verification.
// The written box tree is intentionally codec-agnostic (no avc1/mp4a sample
// entry payloads): the goal here is a deterministic, parseable container whose
// sample tables, chunk offsets and keyframe (stss) index survive a full
// write/read cycle, not interoperability with external players.
package container

import (
	"github.com/abema/go-mp4"

	"mediapipeline/internal/index"
	"mediapipeline/internal/mediaerr"
)

// TrackConfig describes one track to be written.
type TrackConfig struct {
	TrackID   uint32
	Timescale uint32
	IsVideo   bool
	// Samples holds each sample's payload in presentation order.
	Samples [][]byte
	// Delta is the per-sample duration in track timescale units; a single
	// stts entry with a constant delta is sufficient for the fixtures.
	Delta uint32
	// Keyframes holds the validated keyframe index (nil for audio).
	Keyframes *index.Index
}

// identityMatrix is the standard MP4 unity matrix for mvhd/tkhd.
var identityMatrix = [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}

// Write serializes the tracks into an MP4 byte stream. ftyp and moov are
// written first (with zeroed stco placeholders), then a single mdat holding
// every sample back to back; the chunk offsets are backfilled at the end.
func Write(tracks []TrackConfig) ([]byte, *mediaerr.Error) {
	if len(tracks) == 0 {
		return nil, mediaerr.New(mediaerr.StageContainer, mediaerr.KindInvalidInput,
			"at least one track is required")
	}
	raw := make([]byte, 0, 4096)
	ws := &writeSeeker{buf: raw}
	w := mp4.NewWriter(ws)
	st := &writerState{}
	if merr := writeFtyp(w); merr != nil {
		return nil, merr
	}
	if merr := writeMoov(w, tracks, st); merr != nil {
		return nil, merr
	}
	// The mdat header is 8 bytes; samples begin immediately after it.
	mdatStart := uint64(len(ws.buf)) + 8
	if merr := writeMdat(w, ws, tracks, st, mdatStart); merr != nil {
		return nil, merr
	}
	return ws.bytes(), nil
}

// wrapBox converts a go-mp4 error into the structured pipeline error model.
func wrapBox(msg string, err error) *mediaerr.Error {
	return mediaerr.Wrap(mediaerr.StageContainer, mediaerr.KindInvalidInput, msg, err)
}

func writeFtyp(w *mp4.Writer) *mediaerr.Error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()}); err != nil {
		return mediaerr.Wrap(mediaerr.StageContainer, mediaerr.KindInvalidInput, "ftyp start", err)
	}
	ftyp := &mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 0x200,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
			{CompatibleBrand: [4]byte{'i', 's', 'o', '2'}},
		},
	}
	if _, err := mp4.Marshal(w, ftyp, mp4.Context{}); err != nil {
		return mediaerr.Wrap(mediaerr.StageContainer, mediaerr.KindInvalidInput, "ftyp marshal", err)
	}
	if _, err := w.EndBox(); err != nil {
		return mediaerr.Wrap(mediaerr.StageContainer, mediaerr.KindInvalidInput, "ftyp end", err)
	}
	return nil
}

func writeMoov(w *mp4.Writer, tracks []TrackConfig, st *writerState) *mediaerr.Error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		return wrapBox("moov start", err)
	}
	if merr := writeMvhd(w, tracks); merr != nil {
		return merr
	}
	for _, tc := range tracks {
		if merr := writeTrak(w, tc, st); merr != nil {
			return merr
		}
	}
	if _, err := w.EndBox(); err != nil {
		return wrapBox("moov end", err)
	}
	return nil
}

func writeMvhd(w *mp4.Writer, tracks []TrackConfig) *mediaerr.Error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMvhd()}); err != nil {
		return wrapBox("mvhd start", err)
	}
	var maxDuration uint32
	for _, tc := range tracks {
		if d := uint32(len(tc.Samples)) * tc.Delta; d > maxDuration {
			maxDuration = d
		}
	}
	mvhd := &mp4.Mvhd{
		Timescale:   1000,
		DurationV0:  maxDuration,
		Rate:        0x00010000,
		Volume:      0x0100,
		Matrix:      identityMatrix,
		NextTrackID: uint32(len(tracks) + 1),
	}
	if _, err := mp4.Marshal(w, mvhd, mp4.Context{}); err != nil {
		return wrapBox("mvhd marshal", err)
	}
	if _, err := w.EndBox(); err != nil {
		return wrapBox("mvhd end", err)
	}
	return nil
}

func writeTrak(w *mp4.Writer, tc TrackConfig, st *writerState) *mediaerr.Error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()}); err != nil {
		return wrapBox("trak start", err)
	}
	if merr := writeTkhd(w, tc); merr != nil {
		return merr
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdia()}); err != nil {
		return wrapBox("mdia start", err)
	}
	if merr := writeMdhd(w, tc); merr != nil {
		return merr
	}
	if merr := writeHdlr(w, tc); merr != nil {
		return merr
	}
	if merr := writeMinf(w, tc, st); merr != nil {
		return merr
	}
	if _, err := w.EndBox(); err != nil { // mdia
		return wrapBox("mdia end", err)
	}
	if _, err := w.EndBox(); err != nil { // trak
		return wrapBox("trak end", err)
	}
	return nil
}

func writeTkhd(w *mp4.Writer, tc TrackConfig) *mediaerr.Error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTkhd()}); err != nil {
		return wrapBox("tkhd start", err)
	}
	var volume int16
	if !tc.IsVideo {
		volume = 0x0100
	}
	tkhd := &mp4.Tkhd{
		TrackID:    tc.TrackID,
		DurationV0: uint32(len(tc.Samples)) * tc.Delta,
		Volume:     volume,
		Matrix:     identityMatrix,
	}
	if _, err := mp4.Marshal(w, tkhd, mp4.Context{}); err != nil {
		return wrapBox("tkhd marshal", err)
	}
	if _, err := w.EndBox(); err != nil {
		return wrapBox("tkhd end", err)
	}
	return nil
}

func writeMdhd(w *mp4.Writer, tc TrackConfig) *mediaerr.Error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdhd()}); err != nil {
		return wrapBox("mdhd start", err)
	}
	mdhd := &mp4.Mdhd{
		Timescale:  tc.Timescale,
		DurationV0: uint32(len(tc.Samples)) * tc.Delta,
	}
	if _, err := mp4.Marshal(w, mdhd, mp4.Context{}); err != nil {
		return wrapBox("mdhd marshal", err)
	}
	if _, err := w.EndBox(); err != nil {
		return wrapBox("mdhd end", err)
	}
	return nil
}

func writeHdlr(w *mp4.Writer, tc TrackConfig) *mediaerr.Error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeHdlr()}); err != nil {
		return wrapBox("hdlr start", err)
	}
	handler := [4]byte{'s', 'o', 'u', 'n'}
	name := "audio"
	if tc.IsVideo {
		handler = [4]byte{'v', 'i', 'd', 'e'}
		name = "video"
	}
	hdlr := &mp4.Hdlr{HandlerType: handler, Name: name}
	if _, err := mp4.Marshal(w, hdlr, mp4.Context{}); err != nil {
		return wrapBox("hdlr marshal", err)
	}
	if _, err := w.EndBox(); err != nil {
		return wrapBox("hdlr end", err)
	}
	return nil
}
