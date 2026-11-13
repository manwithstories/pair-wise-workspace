package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
)

const (
	Format        = "gkpack-manifest-v1"
	EmptySHA256   = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	RootPrefixLen = 8
)

// BlockRef points at one content-defined chunk. Offset is the byte offset of
// the chunk in the logical file.
type BlockRef struct {
	Hash   string `json:"hash"`
	Offset uint64 `json:"offset"`
	Size   uint32 `json:"size"`
}

// File records reconstruction instructions and an integrity digest for one
// resource. Paths always use forward slashes, including on Windows.
type File struct {
	Path   string     `json:"path"`
	Size   uint64     `json:"size"`
	SHA256 string     `json:"sha256"`
	Blocks []BlockRef `json:"blocks"`
}

// Manifest is the canonical description of a version. Root is derived from
// every field above and never from the host, time, or concurrency settings.
type Manifest struct {
	Format string `json:"format"`
	Root   string `json:"root"`
	Files  []File `json:"files"`
}

// New returns an empty, valid manifest.
func New() Manifest {
	return Manifest{Format: Format, Files: []File{}}
}

// CanonicalBytes returns the stable byte representation used for Root.
func (m Manifest) CanonicalBytes() []byte {
	var buf bytes.Buffer
	writeString(&buf, Format)
	files := append([]File(nil), m.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	writeUint32(&buf, uint32(len(files)))
	for _, f := range files {
		writeString(&buf, f.Path)
		writeUint64(&buf, f.Size)
		writeString(&buf, f.SHA256)
		writeUint32(&buf, uint32(len(f.Blocks)))
		for _, b := range f.Blocks {
			writeString(&buf, b.Hash)
			writeUint64(&buf, b.Offset)
			writeUint32(&buf, b.Size)
		}
	}
	return buf.Bytes()
}

// ComputeRoot calculates the full lower-case SHA-256 root.
func (m Manifest) ComputeRoot() string {
	sum := sha256.Sum256(m.CanonicalBytes())
	return hex.EncodeToString(sum[:])
}

// Finalize sorts files and block references, then stores the computed root.
func (m *Manifest) Finalize() {
	m.Format = Format
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	for i := range m.Files {
		f := &m.Files[i]
		sort.Slice(f.Blocks, func(a, b int) bool { return f.Blocks[a].Offset < f.Blocks[b].Offset })
	}
	m.Root = m.ComputeRoot()
}

// Prefix is the short root form used by human-readable CLI output.
func (m Manifest) Prefix() string {
	if len(m.Root) < RootPrefixLen {
		return m.Root
	}
	return m.Root[:RootPrefixLen]
}

// Validate checks ordering, offsets, digests, and block references.
func (m Manifest) Validate() error {
	if m.Format != Format {
		return fmt.Errorf("unsupported manifest format %q", m.Format)
	}
	if len(m.Root) != sha256.Size*2 {
		return fmt.Errorf("invalid root length %d", len(m.Root))
	}
	prev := ""
	for i, f := range m.Files {
		if f.Path == "" || f.Path[0] == '/' || containsDotDot(f.Path) {
			return fmt.Errorf("invalid file path %q", f.Path)
		}
		if i > 0 && f.Path <= prev {
			return fmt.Errorf("files are not strictly sorted: %q", f.Path)
		}
		prev = f.Path
		if len(f.SHA256) != sha256.Size*2 {
			return fmt.Errorf("%s: invalid file sha256", f.Path)
		}
		var offset uint64
		for _, b := range f.Blocks {
			if b.Hash == "" || len(b.Hash) != sha256.Size*2 {
				return fmt.Errorf("%s: invalid block hash", f.Path)
			}
			if b.Size == 0 {
				return fmt.Errorf("%s: zero-sized block", f.Path)
			}
			if b.Offset != offset {
				return fmt.Errorf("%s: non-contiguous block offset %d, want %d", f.Path, b.Offset, offset)
			}
			offset += uint64(b.Size)
		}
		if offset != f.Size {
			return fmt.Errorf("%s: block sizes total %d, file size %d", f.Path, offset, f.Size)
		}
	}
	if m.Root != m.ComputeRoot() {
		return errors.New("manifest root does not match its contents")
	}
	return nil
}

// FileByPath returns the file entry for path.
func (m Manifest) FileByPath(path string) (File, bool) {
	i := sort.Search(len(m.Files), func(i int) bool { return m.Files[i].Path >= path })
	if i < len(m.Files) && m.Files[i].Path == path {
		return m.Files[i], true
	}
	return File{}, false
}

// BlockSet returns the set of block hashes referenced by the manifest.
func (m Manifest) BlockSet() map[string]bool {
	set := make(map[string]bool)
	for _, f := range m.Files {
		for _, b := range f.Blocks {
			set[b.Hash] = true
		}
	}
	return set
}

// BlockCount returns the number of block references, including reuse.
func (m Manifest) BlockCount() int {
	var n int
	for _, f := range m.Files {
		n += len(f.Blocks)
	}
	return n
}

// TotalSize returns the sum of logical file sizes.
func (m Manifest) TotalSize() uint64 {
	var n uint64
	for _, f := range m.Files {
		n += f.Size
	}
	return n
}

func writeString(buf *bytes.Buffer, value string) {
	writeUint32(buf, uint32(len(value)))
	_, _ = buf.WriteString(value)
}

func writeUint32(buf *bytes.Buffer, value uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], value)
	_, _ = buf.Write(b[:])
}

func writeUint64(buf *bytes.Buffer, value uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], value)
	_, _ = buf.Write(b[:])
}

func containsDotDot(path string) bool {
	start := 0
	for i := 0; i <= len(path); i++ {
		if i == len(path) || path[i] == '/' {
			if i-start == 2 && path[start] == '.' && path[start+1] == '.' {
				return true
			}
			start = i + 1
		}
	}
	return false
}
