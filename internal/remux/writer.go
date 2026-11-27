// Package remux writes the recovered packet stream as Matroska.
//
// Writing is deliberately transactional: FFmpeg muxes into a hidden temporary
// file in the destination directory, the file is flushed and synced, and only
// then renamed over the destination.  Any error, cancellation or short write
// removes the temporary file, so a partial stream is never visible under the
// deliverable name.
package remux

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"

	"github.com/asticode/go-astiav"

	"mediacheck/internal/model"
)

// MaxPacketsPerRun bounds how many packets a single mux may accept.
const MaxPacketsPerRun = 400_000

// ErrAborted is returned when the context is cancelled before the rename.
var ErrAborted = errors.New("remux: aborted before commit")

// Writer muxes packets into a temporary Matroska file and commits atomically.
type Writer struct {
	dest      string
	temp      string
	ofc       *astiav.FormatContext
	io        *astiav.IOContext
	file      *os.File
	packets   *astiav.Packet
	streams   map[model.Track]*astiav.Stream
	infos     map[model.Track]model.StreamInfo
	basetime  map[model.Track]int64
	lastMs    map[model.Track]int64
	started   bool
	committed bool
	closed    bool
	written   int
	bytes     int64
	hasher    hash.Hash
	meta      *astiav.Dictionary
	failed    bool
}

// Open creates the temporary file and writes the Matroska header.  The output
// streams are built from the given descriptors; codec private data is taken
// verbatim from the encoder.  Packets are supplied one at a time with Write,
// so a run never has to hold the whole stream in memory.
func Open(dest string, streams []model.StreamInfo) (*Writer, error) {
	dir := filepath.Dir(dest)
	f, err := os.CreateTemp(dir, ".mediacheck-*.mkv.part")
	if err != nil {
		return nil, fmt.Errorf("remux: creating temporary file: %w", err)
	}
	temp := f.Name()
	// The IO context owns the file descriptor from here on.
	ofc, err := astiav.AllocOutputFormatContext(nil, "matroska", temp)
	if err != nil {
		f.Close()
		os.Remove(temp)
		return nil, fmt.Errorf("remux: allocating output context: %w", err)
	}
	ioCtx, err := astiav.AllocIOContext(64*1024, true, nil,
		func(offset int64, whence int) (int64, error) { return f.Seek(offset, whence) },
		func(b []byte) (int, error) { return f.Write(b) },
	)
	if err != nil {
		ofc.Free()
		f.Close()
		os.Remove(temp)
		return nil, fmt.Errorf("remux: allocating IO context: %w", err)
	}
	ofc.SetFlags(astiav.NewFormatContextFlags(astiav.FormatContextFlagBitexact))
	ofc.SetPb(ioCtx)

	w := &Writer{
		dest:     dest,
		temp:     temp,
		ofc:      ofc,
		io:       ioCtx,
		file:     f,
		packets:  astiav.AllocPacket(),
		streams:  map[model.Track]*astiav.Stream{},
		infos:    map[model.Track]model.StreamInfo{},
		basetime: map[model.Track]int64{},
		lastMs:   map[model.Track]int64{},
		hasher:   sha256.New(),
	}
	// A fixed creation time and bitexact mode make equal inputs produce
	// byte-identical files, which the reproducibility requirement checks.
	// SetMetadata stores the pointer rather than copying, so the dictionary
	// must outlive the context. It is owned by the writer and freed with it.
	meta := astiav.NewDictionary()
	meta.Set("creation_time", "2020-01-01T00:00:00.000000Z", 0)
	ofc.SetMetadata(meta)
	w.meta = meta

	if err := w.addStreams(streams); err != nil {
		w.Abort()
		return nil, err
	}
	if err := ofc.WriteHeader(nil); err != nil {
		w.Abort()
		return nil, fmt.Errorf("remux: writing header: %w", err)
	}
	w.started = true
	return w, nil
}

// Abort releases everything and removes the temporary file.  It is safe to
// call after Commit as well.
func (w *Writer) Abort() {
	if w.closed {
		return
	}
	if w.io != nil {
		w.io.Free()
		w.io = nil
	}
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	w.relinquish()
	os.Remove(w.temp)
	w.closed = true
}

func (w *Writer) relinquish() {
	if w.packets != nil {
		w.packets.Free()
		w.packets = nil
	}
	if w.ofc != nil {
		w.ofc.Free()
		w.ofc = nil
	}
	// avformat_free_context frees the metadata dictionary it was given, so
	// drop the Go reference without freeing it a second time.
	w.meta = nil
	w.closed = true
}

// stripADTS removes the fixed and variable ADTS header so the payload is the
// raw AAC access unit Matroska expects.
func stripADTS(b []byte) ([]byte, error) {
	if len(b) < 7 || b[0] != 0xFF || b[1]&0xF0 != 0xF0 {
		return nil, fmt.Errorf("not an ADTS frame")
	}
	hdr := 7
	if b[1]&0x01 == 0 {
		hdr = 9
	}
	if len(b) <= hdr {
		return nil, fmt.Errorf("ADTS frame carries no payload")
	}
	return b[hdr:], nil
}
