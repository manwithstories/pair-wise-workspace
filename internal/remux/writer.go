package remux

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"

	"mediarun/internal/ingest"
	"mediarun/internal/mapping"
	"mediarun/internal/mediaerr"
)

// Magic and version mark a mediarun remux container. The format is a
// deterministic, self-describing lossless bundle: a JSON header followed by
// length-prefixed sample payloads in mapped target order. Sample bytes are
// copied untouched, so the output is a faithful remux, not a re-encode.
const (
	Magic   = "MRMX"
	Version = uint16(1)
)

// Header is the fixed, canonical-JSON preamble. FrameCountPad is a fixed
// 20-character zero-padded count so Commit can overwrite it in place without
// shifting any frame byte (keeping output layout deterministic).
type Header struct {
	Magic         string             `json:"magic"`
	Version       int                `json:"version"`
	FrameCountPad string             `json:"frame_count_pad"`
	Targets       []mapping.Rule     `json:"targets"`
	Tracks        []ingest.TrackDesc `json:"tracks"`
}

// framePadWidth is the fixed textual width of the frame count field.
const framePadWidth = 20

// FrameHeader prefixes every payload: track ID length-prefixed, sample index,
// track-clock PTS/DTS/duration, data offset, and payload length. Fixed-size
// little-endian integers keep the framing machine-independent.
type FrameHeader struct {
	TrackIDLen uint16
	TrackID    string
	Index      int64
	PTS        int64
	DTS        int64
	Duration   int64
	SrcOffset  int64
	Size       int64
}

// Writer serializes mapped samples into one AtomicWriter deterministically.
type Writer struct {
	out       *AtomicWriter
	started   bool
	hdrLenOff int64 // offset of the 4-byte header length field
	hdrLen    int64 // current header JSON length
	samples   int64
	bytes     int64
}

// NewWriter opens a remux stream over an atomic destination.
func NewWriter(dest string, plan *mapping.Plan) (*Writer, error) {
	aw, err := NewAtomicWriter(dest)
	if err != nil {
		return nil, err
	}
	w := &Writer{out: aw}
	if err := w.writeHeader(plan); err != nil {
		aw.Abort()
		return nil, err
	}
	return w, nil
}

// NewAppendingWriter resumes an existing bundle: it copies the committed
// output into a staging file, parses the header to locate the frame-count
// field and existing frame total, and positions the stream at the end so new
// samples append without altering any prior byte.
func NewAppendingWriter(dest string, plan *mapping.Plan) (*Writer, int64, error) {
	aw, err := NewResumingWriter(dest)
	if err != nil {
		return nil, 0, err
	}
	r, err := OpenReader(dest)
	if err != nil {
		aw.Abort()
		return nil, 0, err
	}
	existing := r.FrameCount()
	r.Close()
	// Locate the frame-count value's absolute offset by re-reading the header
	// length from the top of the copied file.
	if _, err := aw.tmp.Seek(0, io.SeekStart); err != nil {
		aw.Abort()
		return nil, 0, mediaerr.New(mediaerr.CodeIO, err.Error())
	}
	var magic [4]byte
	var lenBuf [4]byte
	if _, err := io.ReadFull(aw.tmp, magic[:]); err != nil {
		aw.Abort()
		return nil, 0, mediaerr.New(mediaerr.CodeTruncatedBox, err.Error())
	}
	if _, err := io.ReadFull(aw.tmp, lenBuf[:]); err != nil {
		aw.Abort()
		return nil, 0, mediaerr.New(mediaerr.CodeTruncatedBox, err.Error())
	}
	hdrLen := int64(binary.LittleEndian.Uint32(lenBuf[:]))
	raw := make([]byte, hdrLen)
	if _, err := io.ReadFull(aw.tmp, raw); err != nil {
		aw.Abort()
		return nil, 0, mediaerr.New(mediaerr.CodeTruncatedKLV, err.Error())
	}
	w := &Writer{
		out:       aw,
		started:   true,
		samples:   existing,
		hdrLenOff: int64(4 + frameCountFieldOffset(raw)),
		hdrLen:    hdrLen,
	}
	if _, err := aw.tmp.Seek(0, io.SeekEnd); err != nil {
		aw.Abort()
		return nil, 0, mediaerr.New(mediaerr.CodeIO, err.Error())
	}
	return w, existing, nil
}

func (w *Writer) writeHeader(plan *mapping.Plan) error {
	hdr := Header{
		Magic: Magic, Version: int(Version),
		FrameCountPad: fmt.Sprintf("%0*d", framePadWidth, 0),
		Tracks:        plan.TrackDescs(),
	}
	for _, t := range plan.Targets {
		hdr.Targets = append(hdr.Targets, mapping.Rule{Target: t.Role, Source: t.Source.ID})
	}
	raw, err := json.Marshal(hdr) // deterministic: struct order, no maps
	if err != nil {
		return mediaerr.New(mediaerr.CodeIO, err.Error())
	}
	if _, err := w.out.Write([]byte(Magic)); err != nil {
		return err
	}
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(raw)))
	if _, err := w.out.Write(lenBuf[:]); err != nil {
		return err
	}
	// Remember where the JSON starts and where the padded count sits inside
	// it, so Commit can overwrite the count with no layout change.
	w.hdrLenOff = int64(4 + frameCountFieldOffset(raw))
	w.hdrLen = int64(len(raw))
	if _, err := w.out.Write(raw); err != nil {
		return err
	}
	w.started = true
	return nil
}

// frameCountFieldOffset returns the byte offset of the padded count value
// inside the marshalled header JSON (right after its key and opening quote).
func frameCountFieldOffset(raw []byte) int {
	key := []byte(`"frame_count_pad":"`)
	idx := bytes.Index(raw, key)
	if idx < 0 {
		panic("remux: frame count key missing from header")
	}
	return idx + len(key)
}

// WriteSample frames one sample event and streams its payload.
func (w *Writer) WriteSample(ev *ingest.SampleEvent) error {
	id := []byte(ev.TrackID)
	if len(id) > 0xffff {
		return mediaerr.New(mediaerr.CodeInvalidConfig,
			fmt.Sprintf("track id too long: %d", len(id)))
	}
	var buf [50]byte
	binary.LittleEndian.PutUint16(buf[0:2], uint16(len(id)))
	binary.LittleEndian.PutUint64(buf[2:10], uint64(ev.Index))
	binary.LittleEndian.PutUint64(buf[10:18], uint64(ev.PTS))
	binary.LittleEndian.PutUint64(buf[18:26], uint64(ev.DTS))
	binary.LittleEndian.PutUint64(buf[26:34], uint64(ev.Duration))
	binary.LittleEndian.PutUint64(buf[34:42], uint64(ev.DataOffset))
	binary.LittleEndian.PutUint64(buf[42:50], uint64(ev.Size))
	if _, err := w.out.Write(buf[:]); err != nil {
		return err
	}
	if _, err := w.out.Write(id); err != nil {
		return err
	}
	n, err := copyAll(w.out, ev.Payload, ev.Size)
	if err != nil {
		return err
	}
	w.samples++
	w.bytes += n
	return nil
}

// Commit finalizes the bundle: it stamps the real frame count into the
// fixed-width header field, then atomically renames and returns the hash.
func (w *Writer) Commit() (hash string, err error) {
	pad := []byte(fmt.Sprintf("%0*d", framePadWidth, w.samples))
	// File layout: magic(4) + length(4) + JSON. hdrLenOff already stores the
	// absolute file offset of the padded value (4 + offset within JSON).
	abs := int64(8) + (w.hdrLenOff - 4)
	if err := w.out.WriteAt(pad, abs); err != nil {
		w.Abort()
		return "", err
	}
	// Hash the fully patched final file before it is renamed away.
	hash, err = w.out.HashFile()
	if err != nil {
		w.Abort()
		return "", err
	}
	if _, err := w.out.Commit(); err != nil {
		return "", err
	}
	return hash, nil
}

// Abort discards the staged output.
func (w *Writer) Abort() { w.out.Abort() }

// Samples returns how many samples have been written.
func (w *Writer) Samples() int64 { return w.samples }

// Bytes returns total payload bytes written.
func (w *Writer) Bytes() int64 { return w.bytes }
