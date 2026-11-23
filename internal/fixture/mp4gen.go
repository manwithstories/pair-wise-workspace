// Package fixture generates deterministic local MP4 material and RTP packet
// traces for the archive demos and tests. Everything is produced in-process;
// no external files are referenced.
package fixture

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	mp4 "github.com/abema/go-mp4"
)

// Clip describes one generated MP4 track.
type Clip struct {
	TrackID   uint32
	Timescale uint32
	Origin    int64 // media time of the first sample (non-zero for broadcast masters)
	Samples   int
	Delta     uint32 // media ticks between samples
	KeyEvery  int    // every Nth sample (1-based) is a sync sample
	Kind      string // "video" or "audio"
	Payload   int    // bytes written per sample
	// ChunkSamples groups this many samples per chunk. Zero means one sample
	// per chunk; a larger value shrinks the chunk offset table.
	ChunkSamples int
	// ConstSize writes stsz as a single sample_size value instead of a
	// per-sample table, keeping moov compact for long tracks.
	ConstSize bool
	// PadBeforeMdat inserts a free box of this many bytes between moov and
	// mdat. It lets callers place the media payload at a chosen absolute
	// offset so byte-accurate damage can be injected right after it.
	PadBeforeMdat int
}

// Layout reports where the sample table put things, for indexing assertions.
type Layout struct {
	Path        string
	Sizes       []uint32
	Offsets     []uint64
	SyncSamples []uint32 // 1-based sample numbers
	SampleTimes []int64  // absolute media time of each sample
	MdatOffset  uint64
}

// Generate writes a single-track, flat (non-fragmented) MP4 to path and
// returns the layout it produced. The file is valid for go-mp4 parsing.
func Generate(path string, c Clip) (*Layout, error) {
	if c.Samples <= 0 || c.Timescale == 0 || c.Delta == 0 {
		return nil, fmt.Errorf("fixture: invalid clip parameters")
	}
	if c.KeyEvery <= 0 {
		c.KeyEvery = 1
	}
	if c.ChunkSamples <= 0 {
		c.ChunkSamples = 1
	}
	if c.ConstSize {
		c.Payload -= c.Payload % 1 // constant sizes ignore the i%7 variation
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return nil, err
	}

	sizes := make([]uint32, c.Samples)
	offsets := make([]uint64, c.Samples)
	times := make([]int64, c.Samples)
	keys := make([]uint32, 0, c.Samples/c.KeyEvery+1)
	for i := 0; i < c.Samples; i++ {
		size := c.Payload
		if !c.ConstSize {
			size += i % 7
		}
		sizes[i] = uint32(size)
		times[i] = c.Origin + int64(i)*int64(c.Delta)
		if i%c.KeyEvery == 0 {
			keys = append(keys, uint32(i+1))
		}
	}
	// One sample per chunk keeps the offsets trivially exact.
	layout := &Layout{Sizes: sizes, Offsets: offsets, SyncSamples: keys, SampleTimes: times, Path: path}

	// Write ftyp + moov first, then mdat. Offsets inside stco are absolute,
	// so moov must be written before mdat to know its size. To keep it
	// simple we reserve the mdat header and compute offsets after header size.
	w := mp4.NewWriter(f)

	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()}); err != nil {
		return nil, err
	}
	ftyp := &mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 512,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
			{CompatibleBrand: [4]byte{'i', 's', 'o', '6'}},
			{CompatibleBrand: [4]byte{'m', 'p', '4', '1'}},
		},
	}
	if _, err := mp4.Marshal(w, ftyp, mp4.Context{}); err != nil {
		return nil, err
	}
	if _, err := w.EndBox(); err != nil {
		return nil, err
	}

	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		return nil, err
	}
	if err := writeMvhd(w, c); err != nil {
		return nil, err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()}); err != nil {
		return nil, err
	}
	if err := writeTkhd(w, c); err != nil {
		return nil, err
	}
	if err := writeEdts(w, c); err != nil {
		return nil, err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdia()}); err != nil {
		return nil, err
	}
	if err := writeMdhd(w, c); err != nil {
		return nil, err
	}
	if err := writeHdlr(w, c); err != nil {
		return nil, err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMinf()}); err != nil {
		return nil, err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStbl()}); err != nil {
		return nil, err
	}
	if err := writeStsd(w, c); err != nil {
		return nil, err
	}
	if err := writeStts(w, c); err != nil {
		return nil, err
	}
	// Placeholder tables; stco needs offsets that depend on moov size, so
	// sizes are staged in memory and patched by the caller.
	stsc := &mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{
		{FirstChunk: 1, SamplesPerChunk: uint32(c.ChunkSamples), SampleDescriptionIndex: 1},
	}}
	if err := writeBox(w, stsc); err != nil {
		return nil, err
	}
	if err := writeStss(w, c); err != nil {
		return nil, err
	}
	if err := writeStsz(w, c, sizes); err != nil {
		return nil, err
	}
	chunkCount := (c.Samples + c.ChunkSamples - 1) / c.ChunkSamples
	co64 := &mp4.Co64{EntryCount: uint32(chunkCount), ChunkOffset: make([]uint64, chunkCount)}
	co64Info, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeCo64()})
	if err != nil {
		return nil, err
	}
	if _, err := mp4.Marshal(w, co64, mp4.Context{}); err != nil {
		return nil, err
	}
	co64End, err := w.EndBox()
	if err != nil {
		return nil, err
	}
	if _, err := w.EndBox(); err != nil { // stbl
		return nil, err
	}
	if _, err := w.EndBox(); err != nil { // minf
		return nil, err
	}
	if _, err := w.EndBox(); err != nil { // mdia
		return nil, err
	}
	if _, err := w.EndBox(); err != nil { // trak
		return nil, err
	}
	if _, err := w.EndBox(); err != nil { // moov
		return nil, err
	}

	if c.PadBeforeMdat > 0 {
		// A free box holds no media; it only shifts mdat to the requested
		// absolute offset so callers can damage an exact byte position.
		if err := writeFree(w, c.PadBeforeMdat); err != nil {
			return nil, err
		}
	}

	// mdat payload, chunked per ChunkSamples.
	mdat, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()})
	if err != nil {
		return nil, err
	}
	layout.MdatOffset = mdat.Offset
	pos := mdat.Offset + mdat.HeaderSize
	chunkOffsets := make([]uint64, 0, chunkCount)
	for i := 0; i < c.Samples; i++ {
		if i%c.ChunkSamples == 0 {
			chunkOffsets = append(chunkOffsets, pos)
		}
		offsets[i] = pos
		payload := buildPayload(i, int(sizes[i]))
		if _, err := w.Write(payload); err != nil {
			return nil, err
		}
		pos += uint64(len(payload))
	}
	if _, err := w.EndBox(); err != nil {
		return nil, err
	}

	// Patch co64 with real absolute offsets.
	if _, err := w.Seek(int64(co64Info.Offset+co64Info.HeaderSize), io.SeekStart); err != nil {
		return nil, err
	}
	co64.ChunkOffset = chunkOffsets
	payload, err := canonicalBytes(co64)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(payload); err != nil {
		return nil, err
	}
	_ = co64End
	return layout, nil
}

// writeFree emits a free box of exactly n bytes (including its 8-byte header).
func writeFree(w *mp4.Writer, n int) error {
	if n < 8 {
		n = 8
	}
	body := make([]byte, n-8)
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFree()}); err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}

func canonicalBytes(b mp4.IBox) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := mp4.Marshal(&buf, b, mp4.Context{}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func buildPayload(index, size int) []byte {
	out := make([]byte, size)
	for i := range out {
		out[i] = byte((index*31 + i*17) % 251)
	}
	return out
}

func writeBox(w *mp4.Writer, b mp4.IImmutableBox) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: b.GetType()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, b, mp4.Context{}); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}
