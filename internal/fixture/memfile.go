package fixture

import "io"

// MemFile is an in-memory writable file used by tests. It satisfies
// io.ReaderAt, io.WriteSeeker and Truncate, mirroring *os.File semantics
// without touching the host filesystem.
type MemFile struct {
	b *memBuffer
}

// GenerateFile builds a complete asset inside a MemFile.
func GenerateFile(o Options) (*MemFile, *Layout, error) {
	m := &MemFile{b: &memBuffer{}}
	lay, err := Generate(m.b, o)
	return m, lay, err
}

func (m *MemFile) Write(p []byte) (int, error)        { return m.b.Write(p) }
func (m *MemFile) Seek(off int64, whence int) (int64, error) {
	return m.b.Seek(off, whence)
}
func (m *MemFile) Truncate(size int64) error { return m.b.Truncate(size) }
func (m *MemFile) Len() int                  { return m.b.buf.Len() }
func (m *MemFile) Bytes() []byte             { return m.b.Bytes() }

// ReaderAt returns a bounded ReaderAt over the current contents.
func (m *MemFile) ReaderAt() io.ReaderAt { return &memReaderAt{data: m.b.Bytes()} }

type memReaderAt struct{ data []byte }

func (r *memReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(r.data)) || off < 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// TruncateTailFile is the MemFile variant of TruncateTail.
func TruncateTailFile(m *MemFile, lay *Layout, drop int64) error {
	return TruncateTail(m.b, lay, drop)
}

// CorruptSampleFile is the MemFile variant of CorruptSample.
func CorruptSampleFile(m *MemFile, lay *Layout, idx uint32) error {
	return CorruptSample(m.b, lay, idx)
}

// BloatSTCOCountFile is the MemFile variant of BloatSTCOCount.
func BloatSTCOCountFile(m *MemFile, lay *Layout) error {
	return BloatSTCOCount(m.b, lay)
}

// ForgeOversizeSampleFile is the MemFile variant of ForgeOversizeSample.
func ForgeOversizeSampleFile(m *MemFile, lay *Layout) error {
	return ForgeOversizeSample(m.b, lay)
}

// StreamVideo returns the deterministic video LCG stream selector.
func StreamVideo() uint32 { return streamVideoBase }

// StreamAudio returns the deterministic audio LCG stream selector.
func StreamAudio() uint32 { return streamAudioBase }
