// Package audio normalizes heterogeneous source audio to the pipeline's
// 48 kHz / six-channel layout and provides a strict, chunked WAV container.
package audio

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"pairflow/internal/media"
)

// Format is the WAV stream format this package reads and writes. Only
// uncompressed 16-bit PCM is accepted; anything else is rejected rather than
// silently converted.
type Format struct {
	Rate     int
	Channels int
}

const (
	pcmTag        = 1
	bitsPerSample = 16
)

func (f Format) validate(path string) *media.Error {
	if f.Rate <= 0 {
		return media.Input("wav.rate", "invalid sample rate %d", f.Rate).WithPath(path)
	}
	if f.Channels < 1 || f.Channels > 64 {
		return media.Input("wav.channels", "invalid channel count %d", f.Channels).WithPath(path)
	}
	return nil
}

// Writer appends 16-bit PCM frames and finalizes the RIFF sizes on Close.
type Writer struct {
	f      *os.File
	bw     *bufio.Writer
	format Format
	bytes  int64 // bytes of sample data written
	closed bool
	path   string
}

// Create opens path for a new WAV stream. The caller owns syncing if needed.
func Create(path string, f Format) (*Writer, *media.Error) {
	if err := f.validate(path); err != nil {
		return nil, err
	}
	file, err := os.Create(path)
	if err != nil {
		return nil, media.Wrap(err, media.ClassInput, "wav.create", "cannot create wav").WithPath(path)
	}
	// Reserve the 44-byte canonical PCM header; sizes are patched at Close.
	hdr := make([]byte, 44)
	copy(hdr[0:], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:], 0)
	copy(hdr[8:], "WAVE")
	copy(hdr[12:], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:], 16)
	binary.LittleEndian.PutUint16(hdr[20:], pcmTag)
	binary.LittleEndian.PutUint16(hdr[22:], uint16(f.Channels))
	binary.LittleEndian.PutUint32(hdr[24:], uint32(f.Rate))
	blockAlign := f.Channels * bitsPerSample / 8
	binary.LittleEndian.PutUint32(hdr[28:], uint32(f.Rate*blockAlign))
	binary.LittleEndian.PutUint16(hdr[32:], uint16(blockAlign))
	binary.LittleEndian.PutUint16(hdr[34:], bitsPerSample)
	copy(hdr[36:], "data")
	binary.LittleEndian.PutUint32(hdr[40:], 0)
	if _, err := file.Write(hdr); err != nil {
		_ = file.Close()
		return nil, media.Wrap(err, media.ClassInternal, "wav.header", "cannot write header").WithPath(path)
	}
	return &Writer{f: file, bw: bufio.NewWriterSize(file, 64*1024), format: f, path: path}, nil
}

// Format returns the stream format.
func (w *Writer) Format() Format { return w.format }

// Frames returns the number of sample frames written so far.
func (w *Writer) Frames() int64 { return w.bytes / int64(w.format.Channels*bitsPerSample/8) }

// Append writes whole frames. A partial frame is an input error.
func (w *Writer) Append(frames []int16) *media.Error {
	if w.closed {
		return media.Input("wav.closed", "append after close").WithPath(w.path)
	}
	if len(frames)%w.format.Channels != 0 {
		return media.Input("wav.partial_frame", "sample count %d is not a whole %d-channel frame",
			len(frames), w.format.Channels).WithPath(w.path)
	}
	var buf []byte
	for _, s := range frames {
		buf = append(buf, byte(s), byte(s>>8))
	}
	if _, err := w.bw.Write(buf); err != nil {
		return media.Wrap(err, media.ClassInternal, "wav.write", "cannot write samples").WithPath(w.path)
	}
	w.bytes += int64(len(buf))
	return nil
}

// Close flushes buffered data and patches RIFF/data sizes.
func (w *Writer) Close() *media.Error {
	if w.closed {
		return nil
	}
	w.closed = true
	if err := w.bw.Flush(); err != nil {
		_ = w.f.Close()
		return media.Wrap(err, media.ClassInternal, "wav.flush", "cannot flush").WithPath(w.path)
	}
	if w.bytes+44 > 1<<32-1 {
		_ = w.f.Close()
		return media.Input("wav.too_large", "wav exceeds 4 GiB RIFF limit").WithPath(w.path)
	}
	// Patch RIFF size and data size in place.
	patch := func(off int64, v uint32) *media.Error {
		if _, err := w.f.WriteAt([]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}, off); err != nil {
			return media.Wrap(err, media.ClassInternal, "wav.patch", "cannot patch size").WithPath(w.path)
		}
		return nil
	}
	if err := patch(4, uint32(36+w.bytes)); err != nil {
		_ = w.f.Close()
		return err
	}
	if err := patch(40, uint32(w.bytes)); err != nil {
		_ = w.f.Close()
		return err
	}
	if err := w.f.Close(); err != nil {
		return media.Wrap(err, media.ClassInternal, "wav.close", "cannot close").WithPath(w.path)
	}
	return nil
}

// Reader streams a WAV file in bounded blocks.
type Reader struct {
	f      *os.File
	br     *bufio.Reader
	Format Format
	frames int64
	read   int64
	path   string
}

// Open parses the canonical PCM header. Unsupported encodings are rejected.
func Open(path string) (*Reader, *media.Error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, media.Wrap(err, media.ClassInput, "wav.open", "cannot open wav").WithPath(path)
	}
	hdr := make([]byte, 44)
	if _, err := io.ReadFull(f, hdr); err != nil {
		_ = f.Close()
		return nil, media.Wrap(err, media.ClassInput, "wav.short_header", "truncated wav header").WithPath(path)
	}
	if string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" || string(hdr[12:16]) != "fmt " || string(hdr[36:40]) != "data" {
		_ = f.Close()
		return nil, media.Input("wav.not_pcm_wave", "not a canonical PCM WAVE file").WithPath(path)
	}
	if tag := binary.LittleEndian.Uint16(hdr[20:]); tag != pcmTag {
		_ = f.Close()
		return nil, media.Input("wav.encoding", "unsupported encoding tag %d (only PCM)", tag).WithPath(path)
	}
	if bits := binary.LittleEndian.Uint16(hdr[34:]); bits != bitsPerSample {
		_ = f.Close()
		return nil, media.Input("wav.bits", "unsupported bits per sample %d (only 16)", bits).WithPath(path)
	}
	format := Format{
		Rate:     int(binary.LittleEndian.Uint32(hdr[24:])),
		Channels: int(binary.LittleEndian.Uint16(hdr[22:])),
	}
	if err := format.validate(path); err != nil {
		_ = f.Close()
		return nil, err
	}
	dataBytes := binary.LittleEndian.Uint32(hdr[40:])
	align := int64(format.Channels * bitsPerSample / 8)
	if int64(dataBytes)%align != 0 {
		_ = f.Close()
		return nil, media.Input("wav.partial_frame", "data size %d is not frame aligned", dataBytes).WithPath(path)
	}
	return &Reader{f: f, br: bufio.NewReaderSize(f, 64*1024), Format: format,
		frames: int64(dataBytes) / align, path: path}, nil
}

// Frames is the declared frame count.
func (r *Reader) Frames() int64 { return r.frames }

// ReadFrames reads at most max frames as interleaved samples. It returns
// io.EOF once all declared frames have been consumed.
func (r *Reader) ReadFrames(max int) ([]int16, error) {
	if r.read >= r.frames {
		return nil, io.EOF
	}
	if max <= 0 {
		return nil, fmt.Errorf("audio: max frames must be positive, got %d", max)
	}
	n := int64(max)
	if n > r.frames-r.read {
		n = r.frames - r.read
	}
	samples := make([]int16, int(n)*r.Format.Channels)
	buf := make([]byte, len(samples)*2)
	if _, err := io.ReadFull(r.br, buf); err != nil {
		return nil, fmt.Errorf("audio: read wav %s: %w", r.path, err)
	}
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(buf[2*i:]))
	}
	r.read += n
	return samples, nil
}

// Close releases the file.
func (r *Reader) Close() error { return r.f.Close() }
