package codec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

var (
	ErrBitstream = errors.New("invalid bitstream")
)

// BitWriter emits MSB-first bits and can be backed by any io.Writer.
type BitWriter struct {
	w     io.Writer
	cur   byte
	used  uint8
	bytes int64
	err   error
}

func NewBitWriter(w io.Writer) *BitWriter { return &BitWriter{w: w} }

func (w *BitWriter) WriteBit(bit bool) {
	if w.err != nil {
		return
	}
	w.cur <<= 1
	if bit {
		w.cur |= 1
	}
	w.used++
	if w.used == 8 {
		if err := writeOne(w.w, w.cur); err != nil {
			w.err = err
		}
		w.cur, w.used = 0, 0
		w.bytes++
	}
}

func (w *BitWriter) WriteBits(value uint64, n uint8) {
	if n > 64 {
		w.err = fmt.Errorf("%w: bit count %d", ErrBitstream, n)
		return
	}
	for i := int(n) - 1; i >= 0; i-- {
		w.WriteBit((value>>uint(i))&1 != 0)
	}
}

// WriteUE writes unsigned Exp-Golomb code number v.
func (w *BitWriter) WriteUE(v uint64) {
	if v > (1<<31)-2 {
		w.err = fmt.Errorf("%w: exp-golomb value %d", ErrBitstream, v)
		return
	}
	code := v + 1
	bits := uint8(0)
	for x := code; x > 1; x >>= 1 {
		bits++
	}
	for i := uint8(0); i < bits; i++ {
		w.WriteBit(false)
	}
	w.WriteBits(code, bits+1)
}

// WriteSE maps signed values to Exp-Golomb code numbers.
func (w *BitWriter) WriteSE(v int) {
	if v > 0 {
		w.WriteUE(uint64(2*v - 1))
		return
	}
	w.WriteUE(uint64(-2 * v))
}

// Flush pads the final partial byte with zero bits.
func (w *BitWriter) Flush() error {
	if w.err != nil {
		return w.err
	}
	if w.used > 0 {
		w.cur <<= 8 - w.used
		if err := writeOne(w.w, w.cur); err != nil {
			return err
		}
		w.cur, w.used = 0, 0
		w.bytes++
	}
	return nil
}

func (w *BitWriter) Bytes() int64 { return w.bytes }

func writeOne(w io.Writer, b byte) error {
	var buf [1]byte
	buf[0] = b
	_, err := w.Write(buf[:])
	return err
}

// BitReader consumes MSB-first bits from an in-memory frame payload.
type BitReader struct {
	data []byte
	pos  int
	err  error
}

func NewBitReader(data []byte) *BitReader { return &BitReader{data: data} }

func (r *BitReader) ReadBit() bool {
	if r.err != nil {
		return false
	}
	if r.pos >= len(r.data)*8 {
		r.err = io.ErrUnexpectedEOF
		return false
	}
	bit := r.data[r.pos/8]>>uint(7-r.pos%8)&1 != 0
	r.pos++
	return bit
}

func (r *BitReader) ReadBits(n uint8) uint64 {
	if n > 64 {
		r.err = fmt.Errorf("%w: bit count %d", ErrBitstream, n)
		return 0
	}
	var v uint64
	for i := uint8(0); i < n; i++ {
		v = (v << 1) | boolToUint(r.ReadBit())
	}
	return v
}

// ReadUE decodes an unsigned Exp-Golomb code number.
func (r *BitReader) ReadUE() uint64 {
	if r.err != nil {
		return 0
	}
	zeros := uint8(0)
	for !r.ReadBit() {
		if r.err != nil {
			return 0
		}
		zeros++
		if zeros > 31 {
			r.err = fmt.Errorf("%w: exp-golomb prefix too long", ErrBitstream)
			return 0
		}
	}
	if zeros == 0 {
		return 0
	}
	return (uint64(1) << zeros) - 1 + r.ReadBits(zeros)
}

func (r *BitReader) ReadSE() int {
	code := r.ReadUE()
	if code&1 == 1 {
		return int((code + 1) / 2)
	}
	return -int(code / 2)
}

func (r *BitReader) Err() error { return r.err }

func (r *BitReader) BitsRead() int { return r.pos }

func boolToUint(v bool) uint64 {
	if v {
		return 1
	}
	return 0
}

// WriteRecord emits a four-byte big-endian length followed by payload. Records
// make frame boundaries recoverable without retaining decoded frames.
func WriteRecord(w io.Writer, payload []byte) error {
	if len(payload) > 1<<28 {
		return fmt.Errorf("%w: record too large", ErrBitstream)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if err := writeAllBytes(w, hdr[:]); err != nil {
		return err
	}
	return writeAllBytes(w, payload)
}

// ReadRecord reads one length-prefixed frame payload.
func ReadRecord(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	n, err := io.ReadFull(r, hdr[:])
	if err != nil {
		if errors.Is(err, io.EOF) && n == 0 {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("%w: record header: %v", ErrBitstream, err)
	}
	size := binary.BigEndian.Uint32(hdr[:])
	if size > 1<<28 {
		return nil, fmt.Errorf("%w: record size %d", ErrBitstream, size)
	}
	buf := make([]byte, int(size))
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("%w: record body: %v", ErrBitstream, err)
	}
	return buf, nil
}

func writeAllBytes(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}
