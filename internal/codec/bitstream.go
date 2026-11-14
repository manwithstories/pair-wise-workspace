package codec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"
)

const (
	SegmentMagic      = "RCXSEG01"
	SegmentVersion    = 1
	SegmentHeaderSize = 56
	FrameMetaSize     = 44
)

var ErrCorrupt = errors.New("encoded segment is corrupt")

type BitWriter struct {
	buf   []byte
	cur   byte
	nbits uint8
}

func (w *BitWriter) WriteBit(bit uint32) {
	if bit&1 != 0 {
		w.cur |= 1 << (7 - w.nbits)
	}
	w.nbits++
	if w.nbits == 8 {
		w.buf = append(w.buf, w.cur)
		w.cur, w.nbits = 0, 0
	}
}

func (w *BitWriter) WriteBits(value uint64, n int) {
	for i := n - 1; i >= 0; i-- {
		w.WriteBit(uint32(value>>uint(i)) & 1)
	}
}

func (w *BitWriter) WriteUE(value uint64) {
	code := value + 1
	n := bitsLen(code) - 1
	for i := 0; i < n; i++ {
		w.WriteBit(0)
	}
	w.WriteBits(code, n+1)
}

func (w *BitWriter) WriteSE(value int32) {
	w.WriteUE(signedToUE(value))
}

func (w *BitWriter) Bytes() []byte {
	out := append([]byte(nil), w.buf...)
	if w.nbits != 0 {
		out = append(out, w.cur)
	}
	return out
}

type BitReader struct {
	data   []byte
	bitPos int
}

func NewBitReader(data []byte) *BitReader { return &BitReader{data: data} }

func (r *BitReader) ReadBit() (uint32, error) {
	if r.bitPos >= len(r.data)*8 {
		return 0, fmt.Errorf("%w: truncated bitstream", ErrCorrupt)
	}
	b := r.data[r.bitPos/8]
	bit := uint32(b>>(7-r.bitPos%8)) & 1
	r.bitPos++
	return bit, nil
}

func (r *BitReader) ReadBits(n int) (uint64, error) {
	if n < 0 || n > 64 {
		return 0, fmt.Errorf("%w: invalid bit count", ErrCorrupt)
	}
	var v uint64
	for i := 0; i < n; i++ {
		b, err := r.ReadBit()
		if err != nil {
			return 0, err
		}
		v = v<<1 | uint64(b)
	}
	return v, nil
}

func (r *BitReader) ReadUE() (uint64, error) {
	zeros := 0
	for {
		b, err := r.ReadBit()
		if err != nil {
			return 0, err
		}
		if b != 0 {
			break
		}
		zeros++
		if zeros > 63 {
			return 0, fmt.Errorf("%w: exp-golomb overflow", ErrCorrupt)
		}
	}
	if zeros == 0 {
		return 0, nil
	}
	rest, err := r.ReadBits(zeros)
	if err != nil {
		return 0, err
	}
	return ((uint64(1) << uint(zeros)) - 1) + rest, nil
}

func (r *BitReader) ReadSE() (int32, error) {
	code, err := r.ReadUE()
	if err != nil {
		return 0, err
	}
	return ueToSigned(code), nil
}

func bitsLen(v uint64) int {
	n := 0
	for v > 0 {
		n++
		v >>= 1
	}
	return n
}

func signedToUE(v int32) uint64 {
	if v > 0 {
		return uint64(v)*2 - 1
	}
	return uint64(-int64(v)) * 2
}

func ueToSigned(code uint64) int32 {
	if code&1 == 1 {
		return int32((code + 1) / 2)
	}
	return -int32(code / 2)
}

type SegmentHeader struct {
	Version    uint16
	ID         uint16
	QP         uint16
	Width      uint16
	Height     uint16
	FPS        uint16
	Frames     uint16
	PayloadLen uint64
	PayloadCRC uint32
	PSNR       float64
	HeaderCRC  uint32
}

func (h SegmentHeader) MarshalBinary() []byte {
	out := make([]byte, SegmentHeaderSize)
	copy(out[0:8], SegmentMagic)
	binary.LittleEndian.PutUint16(out[8:10], h.Version)
	binary.LittleEndian.PutUint16(out[10:12], h.ID)
	binary.LittleEndian.PutUint16(out[12:14], h.QP)
	binary.LittleEndian.PutUint16(out[14:16], h.Width)
	binary.LittleEndian.PutUint16(out[16:18], h.Height)
	binary.LittleEndian.PutUint16(out[18:20], h.FPS)
	binary.LittleEndian.PutUint16(out[20:22], h.Frames)
	binary.LittleEndian.PutUint64(out[24:32], h.PayloadLen)
	binary.LittleEndian.PutUint32(out[32:36], h.PayloadCRC)
	binary.LittleEndian.PutUint64(out[36:44], mathFloat64bits(h.PSNR))
	binary.LittleEndian.PutUint32(out[44:48], crc32.ChecksumIEEE(out[:44]))
	return out
}

func parseSegmentHeader(data []byte) (SegmentHeader, error) {
	if len(data) < SegmentHeaderSize {
		return SegmentHeader{}, fmt.Errorf("%w: truncated segment header", ErrCorrupt)
	}
	if string(data[0:8]) != SegmentMagic {
		return SegmentHeader{}, fmt.Errorf("%w: bad segment magic", ErrCorrupt)
	}
	if crc32.ChecksumIEEE(data[:44]) != binary.LittleEndian.Uint32(data[44:48]) {
		return SegmentHeader{}, fmt.Errorf("%w: segment header checksum mismatch", ErrCorrupt)
	}
	h := SegmentHeader{
		Version:    binary.LittleEndian.Uint16(data[8:10]),
		ID:         binary.LittleEndian.Uint16(data[10:12]),
		QP:         binary.LittleEndian.Uint16(data[12:14]),
		Width:      binary.LittleEndian.Uint16(data[14:16]),
		Height:     binary.LittleEndian.Uint16(data[16:18]),
		FPS:        binary.LittleEndian.Uint16(data[18:20]),
		Frames:     binary.LittleEndian.Uint16(data[20:22]),
		PayloadLen: binary.LittleEndian.Uint64(data[24:32]),
		PayloadCRC: binary.LittleEndian.Uint32(data[32:36]),
		PSNR:       mathFloat64frombits(binary.LittleEndian.Uint64(data[36:44])),
		HeaderCRC:  binary.LittleEndian.Uint32(data[44:48]),
	}
	if h.Version != SegmentVersion || h.Width == 0 || h.Height == 0 || h.FPS == 0 || h.Frames == 0 {
		return SegmentHeader{}, fmt.Errorf("%w: invalid segment header", ErrCorrupt)
	}
	return h, nil
}

type frameMeta struct {
	index       uint16
	payloadLen  uint32
	payloadCRC  uint32
	decodedHash [32]byte
}

func putFrameMeta(dst []byte, m frameMeta) {
	binary.LittleEndian.PutUint16(dst[0:2], m.index)
	binary.LittleEndian.PutUint32(dst[4:8], m.payloadLen)
	binary.LittleEndian.PutUint32(dst[8:12], m.payloadCRC)
	copy(dst[12:44], m.decodedHash[:])
}

func getFrameMeta(src []byte) frameMeta {
	m := frameMeta{
		index:      binary.LittleEndian.Uint16(src[0:2]),
		payloadLen: binary.LittleEndian.Uint32(src[4:8]),
		payloadCRC: binary.LittleEndian.Uint32(src[8:12]),
	}
	copy(m.decodedHash[:], src[12:44])
	return m
}

// WriteAtomic writes data to a temporary file in the destination directory,
// fsyncs it, then atomically renames it into place.
func WriteAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".rcxseg-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func mathFloat64bits(v float64) uint64 { return math.Float64bits(v) }

func mathFloat64frombits(v uint64) float64 { return math.Float64frombits(v) }
