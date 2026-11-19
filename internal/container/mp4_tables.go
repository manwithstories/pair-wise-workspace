package container

import (
	"bytes"
	"io"

	"github.com/abema/go-mp4"

	"mediapipeline/internal/mediaerr"
)

// writeSeeker adapts a byte buffer to io.WriteSeeker and supports backward
// patching: after the full layout is known, Seek(SeekStart) followed by Write
// overwrites placeholder fields (the stco chunk offset) in place.
type writeSeeker struct {
	buf []byte
	pos int64
}

func newWriteSeeker(b *bytes.Buffer) io.WriteSeeker {
	return &writeSeeker{buf: b.Bytes()}
}

func (s *writeSeeker) Write(p []byte) (int, error) {
	end := s.pos + int64(len(p))
	if int64(cap(s.buf)) < end {
		grown := make([]byte, end, end*2)
		copy(grown, s.buf[:len(s.buf)])
		s.buf = grown
	}
	if int64(len(s.buf)) < end {
		s.buf = s.buf[:end]
	}
	copy(s.buf[s.pos:end], p)
	s.pos = end
	return len(p), nil
}

func (s *writeSeeker) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = s.pos + offset
	case io.SeekEnd:
		target = int64(len(s.buf)) + offset
	default:
		return 0, errSeekUnsupported
	}
	if target < 0 || target > int64(len(s.buf)) {
		return 0, errSeekUnsupported
	}
	s.pos = target
	return target, nil
}

// bytes returns the serialized content and must be called after all writes.
func (s *writeSeeker) bytes() []byte { return s.buf[:s.pos] }

// errSeekUnsupported marks an invalid seek on the in-memory writer.
var errSeekUnsupported = mediaerr.New(
	mediaerr.StageContainer, mediaerr.KindInvalidInput, "invalid seek on in-memory MP4 writer")

// offsetPatch records one stco field to backfill after mdat layout is known.
type offsetPatch struct {
	// position is the absolute file position of the 4-byte offset field.
	position int64
}

// writerState carries per-Write backfill slots; one instance per Write call.
type writerState struct {
	patches []offsetPatch
}

func writeMinf(w *mp4.Writer, tc TrackConfig, st *writerState) *mediaerr.Error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMinf()}); err != nil {
		return wrapBox("minf start", err)
	}
	mediaBox := mp4.BoxTypeSmhd()
	if tc.IsVideo {
		mediaBox = mp4.BoxTypeVmhd()
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mediaBox}); err != nil {
		return wrapBox("media header start", err)
	}
	var payload mp4.IBox
	if tc.IsVideo {
		payload = &mp4.Vmhd{}
	} else {
		payload = &mp4.Smhd{}
	}
	if _, err := mp4.Marshal(w, payload, mp4.Context{}); err != nil {
		return wrapBox("media header marshal", err)
	}
	if _, err := w.EndBox(); err != nil {
		return wrapBox("media header end", err)
	}
	if merr := writeDinf(w); merr != nil {
		return merr
	}
	if merr := writeStbl(w, tc, st); merr != nil {
		return merr
	}
	if _, err := w.EndBox(); err != nil { // minf
		return wrapBox("minf end", err)
	}
	return nil
}

func writeDinf(w *mp4.Writer) *mediaerr.Error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeDinf()}); err != nil {
		return wrapBox("dinf start", err)
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeDref()}); err != nil {
		return wrapBox("dref start", err)
	}
	if _, err := mp4.Marshal(w, &mp4.Dref{EntryCount: 1}, mp4.Context{}); err != nil {
		return wrapBox("dref marshal", err)
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeUrl()}); err != nil {
		return wrapBox("url start", err)
	}
	url := &mp4.Url{}
	url.SetFlags(mp4.UrlSelfContained)
	if _, err := mp4.Marshal(w, url, mp4.Context{}); err != nil {
		return wrapBox("url marshal", err)
	}
	if _, err := w.EndBox(); err != nil { // url
		return wrapBox("url end", err)
	}
	if _, err := w.EndBox(); err != nil { // dref
		return wrapBox("dref end", err)
	}
	if _, err := w.EndBox(); err != nil { // dinf
		return wrapBox("dinf end", err)
	}
	return nil
}

func writeStbl(w *mp4.Writer, tc TrackConfig, st *writerState) *mediaerr.Error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStbl()}); err != nil {
		return wrapBox("stbl start", err)
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsd()}); err != nil {
		return wrapBox("stsd start", err)
	}
	if _, err := mp4.Marshal(w, &mp4.Stsd{EntryCount: 0}, mp4.Context{}); err != nil {
		return wrapBox("stsd marshal", err)
	}
	if _, err := w.EndBox(); err != nil {
		return wrapBox("stsd end", err)
	}
	if merr := writeTimeToSample(w, tc); merr != nil {
		return merr
	}
	if merr := writeStss(w, tc); merr != nil {
		return merr
	}
	if merr := writeSampleSizes(w, tc); merr != nil {
		return merr
	}
	if merr := writeSampleToChunk(w, tc); merr != nil {
		return merr
	}
	if merr := writeChunkOffset(w, tc, st); merr != nil {
		return merr
	}
	if _, err := w.EndBox(); err != nil { // stbl
		return wrapBox("stbl end", err)
	}
	return nil
}

func writeTimeToSample(w *mp4.Writer, tc TrackConfig) *mediaerr.Error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStts()}); err != nil {
		return wrapBox("stts start", err)
	}
	stts := &mp4.Stts{
		EntryCount: 1,
		Entries:    []mp4.SttsEntry{{SampleCount: uint32(len(tc.Samples)), SampleDelta: tc.Delta}},
	}
	if _, err := mp4.Marshal(w, stts, mp4.Context{}); err != nil {
		return wrapBox("stts marshal", err)
	}
	if _, err := w.EndBox(); err != nil {
		return wrapBox("stts end", err)
	}
	return nil
}

func writeStss(w *mp4.Writer, tc TrackConfig) *mediaerr.Error {
	if tc.Keyframes == nil || len(tc.Keyframes.Entries()) == 0 {
		return nil // audio tracks and no-keyframe video omit stss entirely
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStss()}); err != nil {
		return wrapBox("stss start", err)
	}
	entries := tc.Keyframes.Entries()
	nums := make([]uint32, len(entries))
	for i, e := range entries {
		nums[i] = e.SampleNumber
	}
	stss := &mp4.Stss{EntryCount: uint32(len(nums)), SampleNumber: nums}
	if _, err := mp4.Marshal(w, stss, mp4.Context{}); err != nil {
		return wrapBox("stss marshal", err)
	}
	if _, err := w.EndBox(); err != nil {
		return wrapBox("stss end", err)
	}
	return nil
}

func writeSampleSizes(w *mp4.Writer, tc TrackConfig) *mediaerr.Error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsz()}); err != nil {
		return wrapBox("stsz start", err)
	}
	sizes := make([]uint32, len(tc.Samples))
	for i, s := range tc.Samples {
		sizes[i] = uint32(len(s))
	}
	stsz := &mp4.Stsz{SampleSize: 0, SampleCount: uint32(len(sizes)), EntrySize: sizes}
	if _, err := mp4.Marshal(w, stsz, mp4.Context{}); err != nil {
		return wrapBox("stsz marshal", err)
	}
	if _, err := w.EndBox(); err != nil {
		return wrapBox("stsz end", err)
	}
	return nil
}

func writeSampleToChunk(w *mp4.Writer, tc TrackConfig) *mediaerr.Error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsc()}); err != nil {
		return wrapBox("stsc start", err)
	}
	stsc := &mp4.Stsc{
		EntryCount: 1,
		Entries:    []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: uint32(len(tc.Samples)), SampleDescriptionIndex: 1}},
	}
	if _, err := mp4.Marshal(w, stsc, mp4.Context{}); err != nil {
		return wrapBox("stsc marshal", err)
	}
	if _, err := w.EndBox(); err != nil {
		return wrapBox("stsc end", err)
	}
	return nil
}

// writeChunkOffset emits stco with a zero placeholder and records the field
// position for backfilling once the mdat start is known.
func writeChunkOffset(w *mp4.Writer, tc TrackConfig, st *writerState) *mediaerr.Error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStco()}); err != nil {
		return wrapBox("stco start", err)
	}
	stco := &mp4.Stco{EntryCount: 1, ChunkOffset: []uint32{0}}
	if _, err := mp4.Marshal(w, stco, mp4.Context{}); err != nil {
		return wrapBox("stco marshal", err)
	}
	if _, err := w.EndBox(); err != nil {
		return wrapBox("stco end", err)
	}
	// The placeholder 4-byte offset is the final field of the box; capture
	// its absolute position via the underlying seeker.
	pos, perr := w.Seek(0, io.SeekCurrent)
	if perr != nil {
		return wrapBox("stco position", perr)
	}
	st.patches = append(st.patches, offsetPatch{position: pos - 4})
	return nil
}
