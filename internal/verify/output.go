// Package verify writes and independently re-reads the normalized archive
// output. The container is deterministic so repeated runs of the same input
// produce the same SHA-256.
package verify

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"

	"archivecheck/internal/fault"
	"archivecheck/internal/index"
	"archivecheck/internal/repair"
)

// Magic identifies the normalized archive container.
var Magic = [8]byte{'A', 'C', 'X', 'A', 'R', 'C', 'H', '1'}

const (
	tagFragment = byte(0x01)
	tagEnd      = byte(0x00)
)

// Writer appends normalized fragment records and maintains the running digest.
type Writer struct {
	f   *os.File
	h   hash.Hash
	n   int64
	tmp []byte
}

// NewWriter creates or truncates path and writes the container magic.
func NewWriter(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fault.Wrap(err, fault.KindOutput, path, "", 0, "cannot create output")
	}
	w := &Writer{f: f, h: sha256.New(), tmp: make([]byte, 32*1024)}
	if _, err := w.write(Magic[:]); err != nil {
		f.Close()
		return nil, err
	}
	return w, nil
}

// ResumeWriter appends to an existing output after validating that its prefix
// matches the committed digest. The extra uncommitted tail is truncated.
func ResumeWriter(path, committedDigest string, committedBytes int64) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return nil, fault.Wrap(err, fault.KindOutput, path, "", 0, "cannot open existing output")
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fault.Wrap(err, fault.KindOutput, path, "", 0, "cannot stat output")
	}
	if info.Size() < committedBytes {
		f.Close()
		return nil, fault.New(fault.KindOutput, path, "", info.Size(),
			fmt.Sprintf("output is %d bytes, checkpoint requires %d", info.Size(), committedBytes))
	}
	h := sha256.New()
	if _, err := io.CopyN(h, f, committedBytes); err != nil {
		f.Close()
		return nil, fault.Wrap(err, fault.KindOutput, path, "", 0, "cannot hash committed prefix")
	}
	if hex.EncodeToString(h.Sum(nil)) != committedDigest {
		f.Close()
		return nil, fault.New(fault.KindOutput, path, "", committedBytes,
			"committed output digest does not match the checkpoint; refusing to overwrite")
	}
	if info.Size() > committedBytes {
		if err := f.Truncate(committedBytes); err != nil {
			f.Close()
			return nil, fault.Wrap(err, fault.KindOutput, path, "", committedBytes, "cannot drop uncommitted tail")
		}
	}
	if _, err := f.Seek(committedBytes, io.SeekStart); err != nil {
		f.Close()
		return nil, fault.Wrap(err, fault.KindOutput, path, "", committedBytes, "cannot seek output")
	}
	return &Writer{f: f, h: h, n: committedBytes, tmp: make([]byte, 32*1024)}, nil
}

func (w *Writer) write(p []byte) (int, error) {
	n, err := w.f.Write(p)
	if n > 0 {
		w.h.Write(p[:n])
		w.n += int64(n)
	}
	if err != nil {
		return n, fault.Wrap(err, fault.KindOutput, "", "", w.n, "output write failed")
	}
	return n, nil
}

// Sync flushes committed bytes to stable storage without sealing the file.
func (w *Writer) Sync() error {
	if err := w.f.Sync(); err != nil {
		return fault.Wrap(err, fault.KindOutput, "", "", w.n, "cannot fsync output")
	}
	return nil
}

// Size returns the committed byte length written so far.
func (w *Writer) Size() int64 { return w.n }

// Digest snapshots the SHA-256 of everything written so far.
func (w *Writer) Digest() string { return hex.EncodeToString(w.h.Sum(nil)) }

// WriteFragment streams one fragment-track record, copying sample payloads
// from src. It returns index entries whose offsets are output-absolute. The
// interval id and seekability are assigned later by index.Normalize.
func (w *Writer) WriteFragment(ft repair.FragmentTimeline, src io.ReaderAt) ([]index.Entry, error) {
	var head bytes.Buffer
	head.WriteByte(tagFragment)
	writeString(&head, ft.Segment)
	writeU32(&head, ft.TrackID)
	writeU32(&head, ft.Sequence)
	writeU32(&head, ft.Timescale)
	writeU64(&head, ft.OriginalBase)
	writeU64(&head, ft.RepairedBase)
	head.WriteByte(reasonCode(ft.Reason))
	writeU32(&head, uint32(len(ft.Samples)))
	if _, err := w.write(head.Bytes()); err != nil {
		return nil, err
	}
	entries := make([]index.Entry, 0, len(ft.Samples))
	for _, s := range ft.Samples {
		var meta [32]byte
		binary.BigEndian.PutUint64(meta[0:8], s.DTS)
		binary.BigEndian.PutUint64(meta[8:16], s.EndDTS)
		binary.BigEndian.PutUint32(meta[16:20], s.Duration)
		binary.BigEndian.PutUint32(meta[20:24], s.Size)
		binary.BigEndian.PutUint32(meta[24:28], s.Flags)
		binary.BigEndian.PutUint32(meta[28:32], uint32(s.CompositionOffset))
		if _, err := w.write(meta[:]); err != nil {
			return nil, err
		}
		offset := w.n
		remaining := int64(s.Size)
		pos := s.Offset
		for remaining > 0 {
			chunk := int64(len(w.tmp))
			if remaining < chunk {
				chunk = remaining
			}
			n, err := src.ReadAt(w.tmp[:chunk], pos)
			if err != nil && err != io.EOF {
				return nil, fault.Wrap(err, fault.KindOutput, ft.Segment, "mdat", pos, "cannot read sample payload")
			}
			if int64(n) != chunk {
				return nil, fault.New(fault.KindShortRead, ft.Segment, "mdat", pos,
					fmt.Sprintf("sample payload short: got %d of %d bytes", n, chunk))
			}
			if _, err := w.write(w.tmp[:chunk]); err != nil {
				return nil, err
			}
			pos += chunk
			remaining -= chunk
		}
		entries = append(entries, index.Entry{
			Segment: ft.Segment, Sequence: ft.Sequence, TrackID: ft.TrackID,
			Timescale: ft.Timescale, DTS: s.DTS, EndDTS: s.EndDTS,
			Offset: offset, Size: s.Size, Sync: s.Sync(), Seekable: s.Sync(),
		})
	}
	return entries, nil
}

// Finalize writes the end marker and content digest, fsyncs, closes the file
// and returns the digest and byte length of the content (everything before the
// trailing digest). The digest is over the same bytes the verifier re-reads.
func (w *Writer) Finalize() (digest string, contentBytes int64, err error) {
	if _, err := w.write([]byte{tagEnd}); err != nil {
		w.f.Close()
		return "", 0, err
	}
	contentBytes = w.n
	sum := w.h.Sum(nil)
	digest = hex.EncodeToString(sum)
	if _, err := w.f.Write(sum); err != nil {
		w.f.Close()
		return "", 0, fault.Wrap(err, fault.KindOutput, "", "", w.n, "cannot write trailer digest")
	}
	w.n += int64(len(sum))
	if err := w.f.Sync(); err != nil {
		w.f.Close()
		return "", 0, fault.Wrap(err, fault.KindOutput, "", "", w.n, "cannot fsync output")
	}
	if err := w.f.Close(); err != nil {
		return "", 0, fault.Wrap(err, fault.KindOutput, "", "", w.n, "cannot close output")
	}
	return digest, contentBytes, nil
}

// Close aborts the writer without a trailer, preserving any committed records.
func (w *Writer) Close() error { return w.f.Close() }

func writeString(b *bytes.Buffer, s string) {
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(s)))
	b.Write(l[:])
	b.WriteString(s)
}

func writeU32(b *bytes.Buffer, v uint32) {
	var x [4]byte
	binary.BigEndian.PutUint32(x[:], v)
	b.Write(x[:])
}

func writeU64(b *bytes.Buffer, v uint64) {
	var x [8]byte
	binary.BigEndian.PutUint64(x[:], v)
	b.Write(x[:])
}

func reasonCode(r repair.Reason) byte {
	switch r {
	case repair.ReasonBackward:
		return 1
	case repair.ReasonGap:
		return 2
	default:
		return 0
	}
}
