package pipeline

import (
	"io"
	"os"

	"mediad/internal/index"
)

// sampleKey uniquely identifies a sample across tracks: each track numbers
// its samples from zero, so track index is required to avoid collisions.
type sampleKey struct {
	track uint32
	index uint32
}

// SampleReader streams original sample bytes straight from the source file.
// Remux never transcodes or synthesizes media, so deliverable bytes come
// from these on-disk ranges. Reads are bounded by the sample table, so an
// out-of-range offset is rejected rather than followed.
type SampleReader struct {
	f      *os.File
	idx    *index.FileIndex
	sample map[sampleKey]index.Sample
}

// NewSampleReader opens the source behind an existing index.
func NewSampleReader(path string, idx *index.FileIndex) (*SampleReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	sr := &SampleReader{f: f, idx: idx, sample: make(map[sampleKey]index.Sample)}
	for ti, tr := range idx.Tracks {
		for _, s := range tr.Samples {
			sr.sample[sampleKey{track: uint32(ti), index: s.Index}] = s
		}
	}
	return sr, nil
}

// Close releases the source handle.
func (sr *SampleReader) Close() error { return sr.f.Close() }

// ReadSample returns the exact on-disk bytes of one sample on a track.
func (sr *SampleReader) ReadSample(track, n uint32) ([]byte, error) {
	s, ok := sr.sample[sampleKey{track: track, index: n}]
	if !ok {
		return nil, nil
	}
	if s.Offset < 0 || s.Size == 0 {
		return []byte{}, nil
	}
	if s.Offset+int64(s.Size) > sr.idx.Size {
		return nil, io.ErrUnexpectedEOF
	}
	buf := make([]byte, s.Size)
	if _, err := sr.f.ReadAt(buf, s.Offset); err != nil {
		return nil, err
	}
	return buf, nil
}
