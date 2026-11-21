package media

import (
	"io"
	"os"
)

// FileSource is the production Source. It is opened read-only; the muxer is
// given a separate output file and never receives this handle for writing.
type FileSource struct {
	f    *os.File
	size int64
	name string
}

// OpenFile opens path for random access without reading it into memory.
func OpenFile(path string) (*FileSource, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, NewError(ClassIO, "cannot open input %s", path).WithOffset(0).WithCause(err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, NewError(ClassIO, "cannot stat input %s", path).WithOffset(0).WithCause(err)
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, NewError(ClassUsage, "input %s is not a regular file", path)
	}
	return &FileSource{f: f, size: info.Size(), name: path}, nil
}

func (s *FileSource) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off > s.size {
		return 0, io.EOF
	}
	return s.f.ReadAt(p, off)
}

func (s *FileSource) Size() int64  { return s.size }
func (s *FileSource) Name() string { return s.name }

// Close releases the file handle.
func (s *FileSource) Close() error { return s.f.Close() }

// BytesSource is an in-memory Source used by tests and the small generated
// fixtures. Large demos use FileSource so memory stays bounded.
type BytesSource struct {
	Data []byte
	Path string
}

func (s *BytesSource) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(s.Data)) {
		return 0, io.EOF
	}
	n := copy(p, s.Data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (s *BytesSource) Size() int64  { return int64(len(s.Data)) }
func (s *BytesSource) Name() string { return s.Path }
