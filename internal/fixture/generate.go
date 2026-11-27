// Package fixture generates self-contained media samples for the media core.
//
// Every fixture is produced from Go code at run time: no external asset files
// are referenced. Generation uses the same go-astiav/FFmpeg installation as
// playback, so the fixtures exercise the real demux path rather than a toy
// container. Each fixture is described by a Manifest that records its
// SHA-256 digest and the structural facts the tests rely on.
package fixture

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
)

// Manifest describes one generated fixture.
type Manifest struct {
	Name        string
	Path        string
	Size        int64
	SHA256      string
	Segments    int
	Seconds     float64
	HasVideo    bool
	HasAudio    bool
	HasSubs     bool
	HasChapters bool
	HasData     bool
	Notes       string
}

// MediaFormat selects the fixture container.
type MediaFormat string

const (
	FormatMP4 MediaFormat = "mp4"
	FormatMKV MediaFormat = "matroska"
	FormatTS  MediaFormat = "mpegts"
)

// Options controls fixture generation.
type Options struct {
	// Dir is the output directory, created when missing.
	Dir string
	// Name is the fixture basename without extension.
	Name string
	// Format selects the container; defaults to FormatMP4.
	Format MediaFormat
	// Segments is the number of one-second output segments to simulate.
	Segments int
	// Fault injects a deliberate defect into the fixture.
	Fault FaultKind
	// MaxRecordBytes caps a single metadata record; larger records are
	// truncated by the FaultTruncate fault.
	MaxRecordBytes int
}

// FaultKind enumerates the corrupt-sample generators.
type FaultKind string

const (
	FaultNone       FaultKind = ""
	FaultTruncate   FaultKind = "truncate"
	FaultTimebase   FaultKind = "timebase"
	FaultStaleIndex FaultKind = "stale-index"
)

// DefaultMaxRecordBytes bounds a single metadata record emitted by fixtures.
const DefaultMaxRecordBytes = 4096

// DigestFile streams a file through SHA-256 and returns its hex digest and size.
func DigestFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// DigestBytes is the SHA-256 hex digest of b.
func DigestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Hasher is a small helper for streamed segment hashing.
type Hasher struct{ h hash.Hash }

// NewHasher returns a SHA-256 streaming hasher.
func NewHasher() *Hasher { return &Hasher{h: sha256.New()} }

// Write adds bytes and reports the count.
func (h *Hasher) Write(p []byte) (int, error) { return h.h.Write(p) }

// Digest returns the hex digest of everything written so far.
func (h *Hasher) Digest() string { return hex.EncodeToString(h.h.Sum(nil)) }

// Extension returns the filename extension for a format.
func (f MediaFormat) Extension() string {
	switch f {
	case FormatMKV:
		return ".mkv"
	case FormatTS:
		return ".ts"
	default:
		return ".mp4"
	}
}

// Validate checks options before generation so failures are input errors, not
// partial writes.
func (o Options) Validate() (Options, error) {
	if o.Dir == "" {
		return o, fmt.Errorf("fixture: output dir is required")
	}
	if o.Name == "" {
		o.Name = "sample"
	}
	if o.Format == "" {
		o.Format = FormatMP4
	}
	switch o.Format {
	case FormatMP4, FormatMKV, FormatTS:
	default:
		return o, fmt.Errorf("fixture: unsupported format %q", o.Format)
	}
	if o.Segments <= 0 {
		o.Segments = 8
	}
	if o.Segments > 600 {
		return o, fmt.Errorf("fixture: segments %d exceeds limit 600", o.Segments)
	}
	if o.MaxRecordBytes <= 0 {
		o.MaxRecordBytes = DefaultMaxRecordBytes
	}
	if o.MaxRecordBytes > 1<<20 {
		return o, fmt.Errorf("fixture: record limit %d exceeds 1 MiB", o.MaxRecordBytes)
	}
	switch o.Fault {
	case FaultNone, FaultTruncate, FaultTimebase, FaultStaleIndex:
	default:
		return o, fmt.Errorf("fixture: unknown fault %q", o.Fault)
	}
	return o, nil
}

// Path joins the fixture name, fault suffix and extension.
func (o Options) Path() string {
	name := o.Name
	if o.Fault != FaultNone {
		name += "-" + string(o.Fault)
	}
	return fmt.Sprintf("%s/%s%s", o.Dir, name, o.Format.Extension())
}
