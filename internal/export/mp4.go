package export

import (
	"io"
	"os"
	"path/filepath"

	"medpipe/internal/fault"
	rtpstream "medpipe/internal/rtp"

	mp4 "github.com/abema/go-mp4"
)

// TrackTiming carries one track's clock information into the container.
type TrackTiming struct {
	TrackID       uint32 `json:"track_id"`
	Timescale     uint32 `json:"timescale"`
	DurationTicks uint64 `json:"duration_ticks"`
	Frames        uint64 `json:"frames"`
}

// WriteMP4 writes a local MP4 container: ftyp, moov with per-track
// timing (tkhd/mdhd) and sample tables (stts/stsc/stsz/stco), and an
// mdat holding each track's frames as one chunk of FrameSize samples.
// All timestamps are fixed values, so the bytes are deterministic.
// Written via temp+rename like every other artifact.
func WriteMP4(path string, tracks []TrackTiming, frames []byte) (err error) {
	// Group the frame sequence into one chunk per track.
	trackIdx := map[uint32]int{}
	chunks := make([][]byte, len(tracks))
	for i, t := range tracks {
		trackIdx[t.TrackID] = i
	}
	for off := 0; off+FrameSize <= len(frames); off += FrameSize {
		h, ok := ParseFrameHeader(frames[off : off+FrameSize])
		if !ok {
			return fault.New(fault.KindCorrupt, fault.Unknown, fault.Unknown,
				int64(off), false, "bad frame while building mp4")
		}
		i, ok := trackIdx[h.Track]
		if !ok {
			return fault.New(fault.KindInternal, int64(h.Track), fault.Unknown,
				int64(off), false, "frame for unknown track")
		}
		chunks[i] = append(chunks[i], frames[off:off+FrameSize]...)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".mp4-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	closed := false
	defer func() {
		if !closed {
			tmp.Close()
		}
		os.Remove(tmpName) // no-op once renamed
	}()

	// Pass 1: measure the moov size with placeholder chunk offsets.
	moovSize, err := writeMoov(tmp, tracks, make([]uint32, len(tracks)), 0)
	if err != nil {
		return err
	}
	const ftypSize = 24 // header + brand + version + 2 compatible brands
	const mdatHeader = 8
	stcos := make([]uint32, len(tracks))
	off := uint32(ftypSize) + uint32(moovSize) + mdatHeader
	for i := range tracks {
		stcos[i] = off
		off += uint32(len(chunks[i]))
	}

	// Pass 2: rewrite with real offsets, then append mdat.
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := tmp.Truncate(0); err != nil {
		return err
	}
	if _, err := writeMoov(tmp, tracks, stcos, ftypSize); err != nil {
		return err
	}
	w := mp4.NewWriter(tmp)
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()}); err != nil {
		return err
	}
	for _, chunk := range chunks {
		if _, err := w.Write(chunk); err != nil {
			return err
		}
	}
	if _, err := w.EndBox(); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	closed = true
	return os.Rename(tmpName, path)
}

// writeMoov writes ftyp+moov at the current file offset and returns the
// moov box size in bytes.
func writeMoov(f *os.File, tracks []TrackTiming, stcos []uint32, _ uint32) (uint64, error) {
	w := mp4.NewWriter(f)
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()}); err != nil {
		return 0, err
	}
	if _, err := mp4.Marshal(w, &mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 1,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
			{CompatibleBrand: [4]byte{'m', 'p', '4', '1'}},
		},
	}, mp4.Context{}); err != nil {
		return 0, err
	}
	if _, err := w.EndBox(); err != nil {
		return 0, err
	}

	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		return 0, err
	}
	var maxDur uint64
	for _, t := range tracks {
		if t.DurationTicks > maxDur {
			maxDur = t.DurationTicks
		}
	}
	if err := writeBox(w, &mp4.Mvhd{
		Timescale:   rtpstream.ClockRate,
		DurationV0:  uint32(maxDur),
		Rate:        0x00010000,
		Volume:      0x0100,
		Matrix:      unityMatrix,
		NextTrackID: uint32(len(tracks)) + 1,
	}); err != nil {
		return 0, err
	}
	for i, t := range tracks {
		if err := writeTrak(w, t, stcos[i]); err != nil {
			return 0, err
		}
	}
	bi, err := w.EndBox() // moov
	if err != nil {
		return 0, err
	}
	return bi.Size, nil
}

var unityMatrix = [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}

func writeBox(w *mp4.Writer, box mp4.IImmutableBox) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: box.GetType()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, box, mp4.Context{}); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}

func writeTrak(w *mp4.Writer, t TrackTiming, stco uint32) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()}); err != nil {
		return err
	}
	if err := writeBox(w, &mp4.Tkhd{
		TrackID:    t.TrackID + 1,
		DurationV0: uint32(t.DurationTicks),
		Volume:     0x0100,
		Matrix:     unityMatrix,
	}); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdia()}); err != nil {
		return err
	}
	if err := writeBox(w, &mp4.Mdhd{
		Timescale:  t.Timescale,
		DurationV0: uint32(t.DurationTicks),
		Language:   [3]byte{'u', 'n', 'd'},
	}); err != nil {
		return err
	}
	if err := writeBox(w, &mp4.Hdlr{
		HandlerType: [4]byte{'s', 'o', 'u', 'n'},
		Name:        "medpipe track",
	}); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMinf()}); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStbl()}); err != nil {
		return err
	}
	n := uint32(t.Frames)
	if err := writeBox(w, &mp4.Stts{
		EntryCount: 1,
		Entries:    []mp4.SttsEntry{{SampleCount: n, SampleDelta: rtpstream.TicksPerPacket}},
	}); err != nil {
		return err
	}
	if err := writeBox(w, &mp4.Stsc{
		EntryCount: 1,
		Entries: []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: n,
			SampleDescriptionIndex: 1}},
	}); err != nil {
		return err
	}
	if err := writeBox(w, &mp4.Stsz{
		SampleSize:  FrameSize,
		SampleCount: n,
	}); err != nil {
		return err
	}
	if err := writeBox(w, &mp4.Stco{
		EntryCount:  1,
		ChunkOffset: []uint32{stco},
	}); err != nil {
		return err
	}
	if _, err := w.EndBox(); err != nil { // stbl
		return err
	}
	if _, err := w.EndBox(); err != nil { // minf
		return err
	}
	if _, err := w.EndBox(); err != nil { // mdia
		return err
	}
	_, err := w.EndBox() // trak
	return err
}
