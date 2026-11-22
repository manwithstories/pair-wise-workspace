package remux

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"mediarun/internal/ingest"
	"mediarun/internal/mapping"
	"mediarun/internal/mediaerr"
)

// Frame is one decoded sample record from a remux bundle.
type Frame struct {
	TrackID  string
	Index    int64
	PTS      int64
	DTS      int64
	Duration int64
	SrcOffset int64
	Size     int64
	Payload  io.Reader
}

// Reader parses a committed mediarun bundle for verification and tracing.
type Reader struct {
	file       *os.File
	br         *bufio.Reader
	Header     Header
	seenFrames int64
}

// OpenReader opens and validates the bundle header.
func OpenReader(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(path)
	}
	r := &Reader{file: f, br: bufio.NewReader(f)}
	if err := r.readHeader(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return r, nil
}

func (r *Reader) readHeader() error {
	magic := make([]byte, 4)
	if _, err := io.ReadFull(r.br, magic); err != nil {
		return mediaerr.New(mediaerr.CodeTruncatedBox, "missing bundle magic")
	}
	if string(magic) != Magic {
		return mediaerr.New(mediaerr.CodeUnknownStructure,
			fmt.Sprintf("bad bundle magic %q", string(magic)))
	}
	var lenBuf [4]byte
	if _, err := io.ReadFull(r.br, lenBuf[:]); err != nil {
		return mediaerr.New(mediaerr.CodeTruncatedBox, "missing header length")
	}
	raw := make([]byte, binary.LittleEndian.Uint32(lenBuf[:]))
	if _, err := io.ReadFull(r.br, raw); err != nil {
		return mediaerr.New(mediaerr.CodeTruncatedKLV, "truncated JSON header")
	}
	if err := json.Unmarshal(raw, &r.Header); err != nil {
		return mediaerr.New(mediaerr.CodeUnknownStructure, "bad header JSON: "+err.Error())
	}
	if r.Header.Magic != Magic || r.Header.Version != int(Version) {
		return mediaerr.New(mediaerr.CodeUnknownStructure, "unsupported bundle version")
	}
	return nil
}

// FrameCount returns the number of frames the header promises.
func (r *Reader) FrameCount() int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(r.Header.FrameCountPad), 10, 64)
	return n
}

// Next decodes the next frame or returns io.EOF.
func (r *Reader) Next() (*Frame, error) {
	var buf [50]byte
	if _, err := io.ReadFull(r.br, buf[:]); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, io.EOF
		}
		return nil, mediaerr.New(mediaerr.CodeIO, err.Error())
	}
	idLen := binary.LittleEndian.Uint16(buf[0:2])
	id := make([]byte, idLen)
	if _, err := io.ReadFull(r.br, id); err != nil {
		return nil, mediaerr.New(mediaerr.CodeTruncatedKLV, "truncated track id")
	}
	size := int64(binary.LittleEndian.Uint64(buf[42:50]))
	fr := &Frame{
		TrackID:   string(id),
		Index:     int64(binary.LittleEndian.Uint64(buf[2:10])),
		PTS:       int64(binary.LittleEndian.Uint64(buf[10:18])),
		DTS:       int64(binary.LittleEndian.Uint64(buf[18:26])),
		Duration:  int64(binary.LittleEndian.Uint64(buf[26:34])),
		SrcOffset: int64(binary.LittleEndian.Uint64(buf[34:42])),
		Size:      size,
	}
	// Payload follows immediately; expose a limited reader so callers stream.
	fr.Payload = io.LimitReader(r.br, size)
	r.seenFrames++
	return fr, nil
}

// VerifyEnding must be called after Next returns io.EOF: it confirms the
// number of frames actually read matches the header promise, turning a
// mid-file truncation into a structured TRUNCATED_KLV error.
func (r *Reader) VerifyEnding() error {
	want := r.FrameCount()
	if r.seenFrames != want {
		return mediaerr.New(mediaerr.CodeTruncatedKLV, fmt.Sprintf(
			"bundle truncated: read %d frames, header promises %d",
			r.seenFrames, want))
	}
	return nil
}

// Close releases the file.
func (r *Reader) Close() error { return r.file.Close() }

// Plan reconstructs a mapping.Config from the bundle header for trace checks.
func (r *Reader) Plan() mapping.Config {
	return mapping.Config{Rules: append([]mapping.Rule(nil), r.Header.Targets...)}
}

// TrackDescs exposes the stored track descriptors.
func (r *Reader) TrackDescs() []ingest.TrackDesc { return r.Header.Tracks }
