// Package output remuxes salvageable samples into deliverable MP4 files
// and verifies the result by parsing it back.
package output

import (
	"fmt"
	"io"

	"bcast/media"

	mp4 "github.com/abema/go-mp4"
)

// SampleRef describes one sample to mux: payload location in the source
// plus its timing. Payload bytes are copied, never fabricated.
type SampleRef struct {
	Offset   int64
	Size     uint32
	Duration uint32
	Keyframe bool
}

// TrackMux is one output track.
type TrackMux struct {
	ID        int
	Kind      media.TrackKind
	Timescale uint32
	Samples   []SampleRef
	// CodecEntry is a whole stsd box copied from the source. When empty a
	// synthesized entry is written (fixture generation only).
	CodecEntry []byte
}

// memWS is an in-memory io.WriteSeeker.
type memWS struct {
	buf []byte
	pos int64
}

func (m *memWS) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if end > int64(len(m.buf)) {
		nb := make([]byte, end)
		copy(nb, m.buf)
		m.buf = nb
	}
	copy(m.buf[m.pos:], p)
	m.pos = end
	return len(p), nil
}

func (m *memWS) Seek(off int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = off
	case io.SeekCurrent:
		abs = m.pos + off
	case io.SeekEnd:
		abs = int64(len(m.buf)) + off
	}
	if abs < 0 {
		return 0, fmt.Errorf("negative seek")
	}
	m.pos = abs
	return abs, nil
}

// writeLeaf writes a complete leaf box (header + payload).
func writeLeaf(mw *mp4.Writer, box mp4.IImmutableBox) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: box.GetType()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(mw, box, mp4.Context{}); err != nil {
		return err
	}
	_, err := mw.EndBox()
	return err
}

var matrix = [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}

// MuxBytes muxes tracks into an in-memory MP4 (moov before mdat).
func MuxBytes(src io.ReaderAt, tracks []TrackMux) ([]byte, error) {
	m := &memWS{}
	if err := Mux(m, src, tracks); err != nil {
		return nil, err
	}
	return m.buf, nil
}

// Mux writes a moov-first MP4 to w, streaming sample payloads from src so
// peak memory stays bounded by the largest single sample.
func Mux(w io.WriteSeeker, src io.ReaderAt, tracks []TrackMux) error {
	ftyp := &mp4.Ftyp{MajorBrand: [4]byte{'i', 's', 'o', 'm'}, MinorVersion: 1}
	fs := &memWS{}
	if err := writeLeaf(mp4.NewWriter(fs), ftyp); err != nil {
		return err
	}
	// Pass 1: measure moov with zero chunk offsets.
	zero := make([][]int64, len(tracks))
	for i := range zero {
		zero[i] = []int64{0}
	}
	ms := &memWS{}
	if err := writeMoov(ms, tracks, zero); err != nil {
		return err
	}
	// Chunk offsets: one chunk per track, laid out sequentially in mdat.
	dataStart := int64(len(fs.buf)) + int64(len(ms.buf)) + 8 // 8 = mdat header
	offsets := make([][]int64, len(tracks))
	cur := dataStart
	for i, t := range tracks {
		offsets[i] = []int64{cur}
		for _, s := range t.Samples {
			cur += int64(s.Size)
		}
	}
	if err := writeLeaf(mp4.NewWriter(w), ftyp); err != nil {
		return err
	}
	if err := writeMoov(w, tracks, offsets); err != nil {
		return err
	}
	mw := mp4.NewWriter(w)
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()}); err != nil {
		return err
	}
	for _, t := range tracks {
		for _, s := range t.Samples {
			if _, err := io.CopyN(mw, io.NewSectionReader(src, s.Offset, int64(s.Size)), int64(s.Size)); err != nil {
				return fmt.Errorf("mux sample @%d: %w", s.Offset, err)
			}
		}
	}
	_, err := mw.EndBox()
	return err
}

// writeMoov emits the moov box tree for tracks with the given per-track
// single-chunk offsets.
func writeMoov(w io.WriteSeeker, tracks []TrackMux, chunkOffs [][]int64) error {
	mw := mp4.NewWriter(w)
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		return err
	}
	var maxDur int64
	for _, t := range tracks {
		var d int64
		for _, s := range t.Samples {
			d += int64(s.Duration)
		}
		ms := media.Time{Value: d, Timescale: t.Timescale}.ToTimescale(1000).Value
		if ms > maxDur {
			maxDur = ms
		}
	}
	if err := writeLeaf(mw, &mp4.Mvhd{
		Timescale: 1000, DurationV0: uint32(maxDur),
		Rate: 0x00010000, Volume: 0x0100, Matrix: matrix, NextTrackID: uint32(len(tracks) + 1),
	}); err != nil {
		return err
	}
	for i, t := range tracks {
		if err := writeTrak(mw, t, chunkOffs[i][0]); err != nil {
			return err
		}
	}
	_, err := mw.EndBox()
	return err
}

func writeTrak(mw *mp4.Writer, t TrackMux, chunkOff int64) error {
	var dur int64
	for _, s := range t.Samples {
		dur += int64(s.Duration)
	}
	volume := int16(0)
	if t.Kind == media.TrackAudio {
		volume = 0x0100
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()}); err != nil {
		return err
	}
	if err := writeLeaf(mw, &mp4.Tkhd{
		TrackID: uint32(t.ID), DurationV0: uint32(media.Time{Value: dur, Timescale: t.Timescale}.ToTimescale(1000).Value),
		Volume: volume, Matrix: matrix,
	}); err != nil {
		return err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdia()}); err != nil {
		return err
	}
	if err := writeLeaf(mw, &mp4.Mdhd{
		Timescale: t.Timescale, DurationV0: uint32(dur), Language: [3]byte{'u', 'n', 'd'},
	}); err != nil {
		return err
	}
	hdlr := &mp4.Hdlr{Name: "VideoHandler"}
	if t.Kind == media.TrackAudio {
		hdlr = &mp4.Hdlr{HandlerType: [4]byte{'s', 'o', 'u', 'n'}, Name: "SoundHandler"}
	} else {
		hdlr.HandlerType = [4]byte{'v', 'i', 'd', 'e'}
	}
	if err := writeLeaf(mw, hdlr); err != nil {
		return err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMinf()}); err != nil {
		return err
	}
	if t.Kind == media.TrackAudio {
		if err := writeLeaf(mw, &mp4.Smhd{}); err != nil {
			return err
		}
	} else {
		if err := writeLeaf(mw, &mp4.Vmhd{}); err != nil {
			return err
		}
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeDinf()}); err != nil {
		return err
	}
	if err := writeLeaf(mw, &mp4.Dref{EntryCount: 1}); err != nil {
		return err
	}
	url := &mp4.Url{}
	url.AddFlag(0x000001) // self-contained
	if err := writeLeaf(mw, url); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil { // dinf
		return err
	}
	if err := writeStbl(mw, t, chunkOff); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil { // minf
		return err
	}
	if _, err := mw.EndBox(); err != nil { // mdia
		return err
	}
	_, err := mw.EndBox() // trak
	return err
}

func writeStbl(mw *mp4.Writer, t TrackMux, chunkOff int64) error {
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStbl()}); err != nil {
		return err
	}
	// stts: run-length encode durations.
	var stts mp4.Stts
	for _, s := range t.Samples {
		if n := len(stts.Entries); n > 0 && stts.Entries[n-1].SampleDelta == s.Duration {
			stts.Entries[n-1].SampleCount++
		} else {
			stts.Entries = append(stts.Entries, mp4.SttsEntry{SampleCount: 1, SampleDelta: s.Duration})
		}
	}
	stts.EntryCount = uint32(len(stts.Entries))
	if err := writeLeaf(mw, &stts); err != nil {
		return err
	}
	stsc := &mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{
		{FirstChunk: 1, SamplesPerChunk: uint32(len(t.Samples)), SampleDescriptionIndex: 1},
	}}
	if err := writeLeaf(mw, stsc); err != nil {
		return err
	}
	stsz := &mp4.Stsz{SampleCount: uint32(len(t.Samples))}
	for _, s := range t.Samples {
		stsz.EntrySize = append(stsz.EntrySize, s.Size)
	}
	if err := writeLeaf(mw, stsz); err != nil {
		return err
	}
	if err := writeStsd(mw, t); err != nil {
		return err
	}
	stco := &mp4.Stco{EntryCount: 1, ChunkOffset: []uint32{uint32(chunkOff)}}
	if err := writeLeaf(mw, stco); err != nil {
		return err
	}
	if t.Kind == media.TrackVideo {
		var stss mp4.Stss
		for i, s := range t.Samples {
			if s.Keyframe {
				stss.SampleNumber = append(stss.SampleNumber, uint32(i+1))
			}
		}
		stss.EntryCount = uint32(len(stss.SampleNumber))
		if err := writeLeaf(mw, &stss); err != nil {
			return err
		}
	}
	_, err := mw.EndBox() // stbl
	return err
}
