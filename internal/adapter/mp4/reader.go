// Package mp4 demultiplexes MP4/MOV files into the shared media kernel types.
//
// The reader never decodes: it walks the box tree, expands the sample tables
// (stts/ctts/stsc/stsz/stco/co64/stss), applies edit lists and non-zero media
// start, and emits one Packet per encoded sample. Every packet records the
// byte offset at which it lives in the source so the remux kernel can copy it
// without re-encoding, and so failures can point at a precise byte.
package mp4

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"

	"mediabridge/internal/adapter"
	"mediabridge/internal/fault"
	"mediabridge/internal/media"
)

// Version is the reader revision recorded in provenance and checkpoints.
const Version = 1

// Reader implements adapter.Adapter for MP4/MOV.
type Reader struct{}

// Format returns the manifest format key.
func (r *Reader) Format() string { return "mp4" }

// Version returns the adapter version.
func (r *Reader) Version() int { return Version }

// Name returns a diagnostic label.
func (r *Reader) Name() string { return "mp4/mov demuxer" }

// Sniff reports whether the file starts with an MP4/MOV box header.
func (r *Reader) Sniff(rs io.ReadSeeker) bool {
	pos, err := rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return false
	}
	defer rs.Seek(pos, io.SeekStart)
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return false
	}
	var hdr [8]byte
	if _, err := io.ReadFull(rs, hdr[:]); err != nil {
		return false
	}
	switch string(hdr[4:8]) {
	case "ftyp", "moov", "mdat", "free", "wide", "skip", "uuid", "pnot":
		return true
	}
	return false
}

// Parse implements adapter.Adapter.
func (r *Reader) Parse(src adapter.Source, opts adapter.Options) (*media.Asset, error) {
	f, err := os.Open(src.Path)
	if err != nil {
		return nil, fault.New(fault.CodeCorruptContainer,
			fault.Location{SourcePath: src.Path, AssetID: src.AssetID}.WithOffset(0),
			"open source failed").Wrap(err).WithOp("mp4.Open")
	}
	defer f.Close()

	p := newParser(src, opts, f)
	asset, err := p.parse()
	if err != nil {
		return nil, err
	}
	asset.SourceSHA = digestFile(f)
	return asset, nil
}

// digestFile performs a canonical sequential hash of the whole source. Parsing
// reads are scattered, so a separate pass is required for a stable digest that
// can invalidate checkpoints across runs.
func digestFile(f *os.File) string {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ""
	}
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return ""
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sampleReader reads encoded bytes directly from the source by offset.
type sampleReader struct {
	f *os.File
}

func (s *sampleReader) read(off, size int64) ([]byte, error) {
	if size < 0 || size > media.MaxPacketBytes {
		return nil, fault.New(fault.CodeCorruptContainer,
			fault.Location{}.WithOffset(off), "sample size %d out of range", size)
	}
	buf := make([]byte, size)
	if _, err := s.f.ReadAt(buf, off); err != nil {
		return nil, err
	}
	return buf, nil
}
