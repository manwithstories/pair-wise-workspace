package wav

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
)

// Writer emits a canonical 16-bit PCM WAVE stream. Tests and the simulator use
// it to synthesize fixtures without any external sample files.
type Writer struct {
	w        io.Writer
	fmt      Format
	dataSize uint32
	closed   bool
}

// NewWriter writes the RIFF/fmt headers immediately. The data chunk size is
// patched on Close (buffered writers must be flushed by the caller).
func NewWriter(w io.Writer, sampleRate uint32, channels uint16) (*Writer, error) {
	if channels == 0 {
		return nil, &FormatError{Reason: "channel count must be positive"}
	}
	ww := &Writer{
		w: w,
		fmt: Format{
			SampleRate:    sampleRate,
			Channels:      channels,
			BitsPerSample: BitsPCM16,
		},
	}
	if err := ww.writeHeaderPlaceholder(); err != nil {
		return nil, err
	}
	return ww, nil
}

// FormatError describes an invalid writer configuration.
type FormatError struct{ Reason string }

func (e *FormatError) Error() string { return "wav: " + e.Reason }

func (w *Writer) writeHeaderPlaceholder() error {
	blockAlign := w.fmt.Channels * (w.fmt.BitsPerSample / 8)
	byteRate := w.fmt.SampleRate * uint32(blockAlign)
	buf := &bytes.Buffer{}
	buf.WriteString("RIFF")
	binary.Write(buf, binary.LittleEndian, uint32(0)) // size, patched on Close
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	binary.Write(buf, binary.LittleEndian, uint32(16))
	binary.Write(buf, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(buf, binary.LittleEndian, w.fmt.Channels)
	binary.Write(buf, binary.LittleEndian, w.fmt.SampleRate)
	binary.Write(buf, binary.LittleEndian, byteRate)
	binary.Write(buf, binary.LittleEndian, blockAlign)
	binary.Write(buf, binary.LittleEndian, w.fmt.BitsPerSample)
	buf.WriteString("data")
	binary.Write(buf, binary.LittleEndian, uint32(0)) // data size, patched
	_, err := w.w.Write(buf.Bytes())
	return err
}

// WritePCM appends interleaved 16-bit samples and enforces block alignment.
func (w *Writer) WritePCM(samples []int16) error {
	if w.closed {
		return &FormatError{Reason: "writer already closed"}
	}
	if len(samples)%int(w.fmt.Channels) != 0 {
		return &FormatError{Reason: "pcm length not aligned to channel count"}
	}
	buf := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
	}
	n, err := w.w.Write(buf)
	w.dataSize += uint32(n)
	return err
}

// Close patches the RIFF/data sizes. When the underlying writer implements
// io.WriteSeeker the patch is performed in place; otherwise the caller must
// use WriteBytes which renders a self-contained buffer.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if ws, ok := w.w.(io.WriteSeeker); ok {
		riffSize := uint32(36 + w.dataSize)
		if _, err := ws.Seek(4, io.SeekStart); err != nil {
			return err
		}
		binary.Write(ws, binary.LittleEndian, riffSize)
		if _, err := ws.Seek(40, io.SeekStart); err != nil {
			return err
		}
		binary.Write(ws, binary.LittleEndian, w.dataSize)
	}
	return nil
}

// WriteBytes renders a complete WAV file in memory from interleaved samples.
// Used by tests and deterministic fixture generation.
func WriteBytes(sampleRate uint32, channels uint16, samples []int16) ([]byte, error) {
	buf := &bytes.Buffer{}
	w, err := NewWriter(buf, sampleRate, channels)
	if err != nil {
		return nil, err
	}
	if err := w.WritePCM(samples); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	binary.LittleEndian.PutUint32(out[4:], uint32(len(out)-8))
	binary.LittleEndian.PutUint32(out[40:], uint32(len(out)-44))
	return out, nil
}

// SineSamples synthesizes nFrames of interleaved sine samples across channels,
// with a per-channel phase offset so multichannel data is distinguishable.
func SineFrames(sampleRate uint32, channels uint16, nFrames int, freq float64) []int16 {
	out := make([]int16, nFrames*int(channels))
	for f := 0; f < nFrames; f++ {
		for c := 0; c < int(channels); c++ {
			phase := 2 * math.Pi * freq * float64(f) / float64(sampleRate)
			amp := 0.4 * math.Sin(phase+float64(c)*math.Pi/4)
			out[f*int(channels)+c] = int16(amp * math.MaxInt16)
		}
	}
	return out
}
