package mp4

import (
	"bytes"

	"github.com/abema/go-mp4"

	"mediamix/internal/timeline"
)

// chunk is one sample placed in mdat.
type chunk struct {
	size  int
	key   bool
	delta uint32 // timescale units
}

// buildMedia lays out mdat bytes and per-track chunk descriptors. All bytes are
// deterministic zero fill: the deliverable is a proxy manifest, not a second
// copy of the camera originals.
func buildMedia(tracks []TrackSamples) (payload []byte, chunks [][]chunk) {
	chunks = make([][]chunk, len(tracks))
	var buf bytes.Buffer
	for ti, t := range tracks {
		list := samplesFor(t)
		prevTicks := uint64(0)
		for _, s := range list {
			size := s.Size
			if size <= 0 {
				size = 1
			}
			tick := uint64(microsToTicks(t.Timescale, s.PTS))
			delta := uint32(tick - prevTicks)
			if delta == 0 && len(chunks[ti]) > 0 {
				delta = 1
			}
			prevTicks = tick
			buf.Write(make([]byte, size))
			chunks[ti] = append(chunks[ti], chunk{size: size, key: s.Key, delta: delta})
		}
	}
	return buf.Bytes(), chunks
}

// samplesFor expands a track's keyframes into a full sample list. Non-sync
// samples are spaced evenly so the declared duration is covered.
func samplesFor(t TrackSamples) []Sample {
	total := t.TotalSamples
	if total < len(t.Keyframes) {
		total = len(t.Keyframes)
	}
	if total == 0 {
		return nil
	}
	if len(t.Keyframes) == 0 {
		out := make([]Sample, total)
		for i := range out {
			out[i] = Sample{PTS: samplePTS(t.Span, i, total), Size: 1}
		}
		return out
	}
	out := make([]Sample, 0, total)
	for i := 0; i < total; i++ {
		if k, ok := keyAtIndex(t.Keyframes, i); ok {
			out = append(out, k)
			continue
		}
		out = append(out, Sample{PTS: samplePTS(t.Span, i, total), Size: 1})
	}
	return out
}

// keyAtIndex maps a sample ordinal to a keyframe by PTS order.
func keyAtIndex(keys []Sample, i int) (Sample, bool) {
	if i < len(keys) {
		return keys[i], true
	}
	return Sample{}, false
}

func samplePTS(span timeline.Span, i, total int) timeline.Micros {
	if total <= 1 {
		return span.Start
	}
	return span.Start + timeline.Micros(int64(span.Duration())*int64(i)/int64(total))
}

func microsToTicks(timescale uint32, us timeline.Micros) int64 {
	if timescale == 0 {
		return 0
	}
	return int64(us) * int64(timescale) / int64(timeline.MicrosPerSecond)
}

// writeFtyp renders the file type box.
func writeFtyp() []byte {
	ftyp := &mp4.Ftyp{MajorBrand: mp4.BrandISO5(), MinorVersion: 0}
	ftyp.AddCompatibleBrand(mp4.BrandISO5())
	ftyp.AddCompatibleBrand(mp4.BrandISO6())
	ftyp.AddCompatibleBrand(mp4.BrandMP41())
	buf := new(bytes.Buffer)
	_ = writeBox(buf, ftyp)
	return buf.Bytes()
}

// writeMoov renders the movie box with chunk offsets starting at base.
func writeMoov(tracks []TrackSamples, chunks [][]chunk, base uint32) ([]byte, error) {
	w := &boxWriter{buf: new(bytes.Buffer)}
	if err := w.startBox(mp4.BoxTypeMoov()); err != nil {
		return nil, err
	}
	if err := w.writeMvhd(tracks); err != nil {
		return nil, err
	}
	offset := base
	for ti, t := range tracks {
		if err := w.writeTrak(uint32(ti+1), t, chunks[ti], &offset); err != nil {
			return nil, err
		}
	}
	if err := w.endBox(); err != nil {
		return nil, err
	}
	return w.buf.Bytes(), nil
}

// writeMvhd writes the movie header sized to the longest track.
func (w *boxWriter) writeMvhd(tracks []TrackSamples) error {
	var maxUs timeline.Micros
	for _, t := range tracks {
		if t.Span.End > maxUs {
			maxUs = t.Span.End
		}
	}
	mvhd := &mp4.Mvhd{
		Timescale:   muxTimescale,
		DurationV0:  uint32(microsToTicks(muxTimescale, maxUs)),
		Rate:        0x00010000,
		Volume:      0x0100,
		Matrix:      identityMatrix(),
		NextTrackID: uint32(len(tracks) + 1),
	}
	return w.writeFullBox(mvhd)
}

// writeTrak writes one track: tkhd, mdia(mdhd, hdlr, minf(stbl)).
func (w *boxWriter) writeTrak(id uint32, t TrackSamples, chunks []chunk, offset *uint32) error {
	if err := w.startBox(mp4.BoxTypeTrak()); err != nil {
		return err
	}
	tkhd := &mp4.Tkhd{
		TrackID:    id,
		DurationV0: uint32(microsToTicks(muxTimescale, t.Span.End)),
		Matrix:     identityMatrix(),
	}
	if t.Kind == timeline.KindAudio {
		tkhd.Volume = 0x0100
	}
	if err := w.writeFullBox(tkhd); err != nil {
		return err
	}
	if err := w.startBox(mp4.BoxTypeMdia()); err != nil {
		return err
	}
	mdhd := &mp4.Mdhd{
		Timescale:  t.Timescale,
		DurationV0: uint32(microsToTicks(t.Timescale, t.Span.End)),
		Language:   [3]byte{'u', 'n', 'd'},
	}
	if err := w.writeFullBox(mdhd); err != nil {
		return err
	}
	handler := [4]byte{'v', 'i', 'd', 'e'}
	if t.Kind == timeline.KindAudio {
		handler = [4]byte{'s', 'o', 'u', 'n'}
	}
	hdlr := &mp4.Hdlr{HandlerType: handler, Name: "mediamix"}
	if err := w.writeBox(hdlr); err != nil {
		return err
	}
	if err := w.startBox(mp4.BoxTypeMinf()); err != nil {
		return err
	}
	if err := w.startBox(mp4.BoxTypeStbl()); err != nil {
		return err
	}
	if err := w.writeSampleTables(chunks, offset); err != nil {
		return err
	}
	if err := w.endBox(); err != nil { // stbl
		return err
	}
	if err := w.endBox(); err != nil { // minf
		return err
	}
	if err := w.endBox(); err != nil { // mdia
		return err
	}
	return w.endBox() // trak
}

// writeSampleTables writes stsd, stts, stsc, stsz and stco. Each sample is its
// own chunk, which keeps the offset table trivial to verify on re-read.
func (w *boxWriter) writeSampleTables(chunks []chunk, offset *uint32) error {
	stsd := &mp4.Stsd{EntryCount: 0}
	if err := w.writeFullBox(stsd); err != nil {
		return err
	}
	// stts: run-length encode equal deltas.
	stts := &mp4.Stts{}
	var entries []mp4.SttsEntry
	for _, c := range chunks {
		delta := c.delta
		if delta == 0 {
			delta = 1
		}
		if n := len(entries); n > 0 && entries[n-1].SampleDelta == delta {
			entries[n-1].SampleCount++
			continue
		}
		entries = append(entries, mp4.SttsEntry{SampleCount: 1, SampleDelta: delta})
	}
	stts.Entries = entries
	stts.EntryCount = uint32(len(entries))
	if err := w.writeFullBox(stts); err != nil {
		return err
	}
	// stsc: one sample per chunk throughout.
	stsc := &mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{
		{FirstChunk: 1, SamplesPerChunk: 1, SampleDescriptionIndex: 1},
	}}
	if err := w.writeFullBox(stsc); err != nil {
		return err
	}
	// stsz: per-sample sizes.
	stsz := &mp4.Stsz{SampleCount: uint32(len(chunks)), EntrySize: make([]uint32, len(chunks))}
	for i, c := range chunks {
		stsz.EntrySize[i] = uint32(c.size)
	}
	if err := w.writeFullBox(stsz); err != nil {
		return err
	}
	// stco: absolute file offsets, computed from the running cursor.
	stco := &mp4.Stco{EntryCount: uint32(len(chunks)), ChunkOffset: make([]uint32, len(chunks))}
	for i, c := range chunks {
		stco.ChunkOffset[i] = *offset
		*offset += uint32(c.size)
	}
	return w.writeFullBox(stco)
}

func makeMdatHeader(size int) []byte {
	total := uint32(size + 8)
	return []byte{
		byte(total >> 24), byte(total >> 16), byte(total >> 8), byte(total),
		'm', 'd', 'a', 't',
	}
}

func identityMatrix() [9]int32 {
	return [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
}
