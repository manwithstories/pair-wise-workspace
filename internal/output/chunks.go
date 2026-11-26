// Package output writes repaired segment chunks as self-contained MP4
// files. Every chunk is committed atomically: write to a temporary
// file, fsync, then rename into place.
package output

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"

	gomp4 "github.com/abema/go-mp4"

	"mediaroute/internal/model"
)

// Chunk is one repaired segment ready to be written.
type Chunk struct {
	Timescale uint32
	Samples   []model.Sample // repaired samples of the segment
	Payload   []byte         // concatenated sample payloads, in sample order
}

// WriteChunk atomically writes a chunk MP4 into dir and returns the
// final path, its SHA-256 and its size in bytes.
func WriteChunk(dir, name string, c Chunk) (string, string, int64, error) {
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", "", 0, model.New(model.CodeIO, "chunk tmp", err)
	}
	tmpPath := tmp.Name()
	if err := writeChunkMP4(tmp, c); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", "", 0, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", "", 0, model.New(model.CodeIO, "chunk fsync", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", "", 0, model.New(model.CodeIO, "chunk close", err)
	}
	sum, size, err := HashFile(tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		return "", "", 0, err
	}
	final := filepath.Join(dir, name)
	if err := os.Rename(tmpPath, final); err != nil {
		os.Remove(tmpPath)
		return "", "", 0, model.New(model.CodeIO, "chunk commit", err)
	}
	return final, sum, size, nil
}

// writeChunkMP4 writes ftyp + mdat + moov with repaired timing tables.
// The moov is placed after mdat so the single chunk offset is known
// before any sample byte is written.
func writeChunkMP4(f *os.File, c Chunk) error {
	w := gomp4.NewWriter(f)
	ctx := gomp4.Context{}
	if _, err := gomp4.Marshal(w, &gomp4.Ftyp{
		MajorBrand:   gomp4.BrandISOM(),
		MinorVersion: 512,
		CompatibleBrands: []gomp4.CompatibleBrandElem{
			{CompatibleBrand: gomp4.BrandISOM()},
			{CompatibleBrand: gomp4.BrandMP41()},
		},
	}, ctx); err != nil {
		return model.New(model.CodeIO, "chunk ftyp", err)
	}
	pos, err := w.Seek(0, io.SeekCurrent)
	if err != nil {
		return model.New(model.CodeIO, "chunk seek", err)
	}
	mdatPayload := uint64(pos) + 8 // 32-bit mdat header
	if _, err := w.StartBox(&gomp4.BoxInfo{Type: gomp4.BoxTypeMdat()}); err != nil {
		return model.New(model.CodeIO, "chunk mdat", err)
	}
	if _, err := w.Write(c.Payload); err != nil {
		return model.New(model.CodeIO, "chunk payload", err)
	}
	if _, err := w.EndBox(); err != nil {
		return model.New(model.CodeIO, "chunk mdat end", err)
	}
	if err := writeMoov(w, c, mdatPayload, ctx); err != nil {
		return err
	}
	return nil
}

func writeMoov(w *gomp4.Writer, c Chunk, mdatPayload uint64, ctx gomp4.Context) error {
	for _, t := range []gomp4.BoxType{
		gomp4.BoxTypeMoov(), gomp4.BoxTypeTrak(), gomp4.BoxTypeMdia(),
	} {
		if _, err := w.StartBox(&gomp4.BoxInfo{Type: t}); err != nil {
			return model.New(model.CodeIO, "chunk moov", err)
		}
	}
	n := len(c.Samples)
	stts, total := buildStts(c.Samples)
	if _, err := gomp4.Marshal(w, &gomp4.Mdhd{
		Timescale:  c.Timescale,
		DurationV0: uint32(total),
	}, ctx); err != nil {
		return model.New(model.CodeIO, "chunk mdhd", err)
	}
	for _, t := range []gomp4.BoxType{gomp4.BoxTypeMinf(), gomp4.BoxTypeStbl()} {
		if _, err := w.StartBox(&gomp4.BoxInfo{Type: t}); err != nil {
			return model.New(model.CodeIO, "chunk stbl", err)
		}
	}
	sizes := make([]uint32, n)
	var sync []uint32
	for i, s := range c.Samples {
		sizes[i] = s.Size
		if s.Keyframe {
			sync = append(sync, uint32(i+1))
		}
	}
	boxes := []gomp4.IImmutableBox{
		&gomp4.Stts{EntryCount: uint32(len(stts)), Entries: stts},
		&gomp4.Stsc{EntryCount: 1, Entries: []gomp4.StscEntry{
			{FirstChunk: 1, SamplesPerChunk: uint32(n), SampleDescriptionIndex: 1}}},
		&gomp4.Stsz{SampleCount: uint32(n), EntrySize: sizes},
		&gomp4.Stss{EntryCount: uint32(len(sync)), SampleNumber: sync},
		&gomp4.Stco{EntryCount: 1, ChunkOffset: []uint32{uint32(mdatPayload)}},
	}
	for _, b := range boxes {
		if _, err := gomp4.Marshal(w, b, ctx); err != nil {
			return model.New(model.CodeIO, "chunk stbl box", err)
		}
	}
	for i := 0; i < 5; i++ { // stbl, minf, mdia, trak, moov
		if _, err := w.EndBox(); err != nil {
			return model.New(model.CodeIO, "chunk moov end", err)
		}
	}
	return nil
}

// buildStts rebuilds decoding-time deltas from repaired DTS values.
// The first sample of a chunk decodes at DTS 0 inside the chunk.
func buildStts(samples []model.Sample) ([]gomp4.SttsEntry, uint64) {
	n := len(samples)
	if n == 0 {
		return []gomp4.SttsEntry{{SampleCount: 0, SampleDelta: 0}}, 0
	}
	deltas := make([]uint32, n)
	for i := 0; i+1 < n; i++ {
		deltas[i] = uint32(samples[i+1].DTS - samples[i].DTS)
	}
	deltas[n-1] = deltas[max(0, n-2)]
	var entries []gomp4.SttsEntry
	for _, d := range deltas {
		if m := len(entries); m > 0 && entries[m-1].SampleDelta == d {
			entries[m-1].SampleCount++
		} else {
			entries = append(entries, gomp4.SttsEntry{SampleCount: 1, SampleDelta: d})
		}
	}
	total := uint64(0)
	for _, d := range deltas {
		total += uint64(d)
	}
	return entries, total
}

// HashFile streams a file and returns its SHA-256 hex digest and size.
func HashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, model.New(model.CodeIO, "hash open "+path, err)
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", 0, model.New(model.CodeIO, "hash read "+path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}
