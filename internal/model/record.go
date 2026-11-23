package model

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
)

// FlagKeyframe marks a video record as an independently decodable keyframe.
const FlagKeyframe uint16 = 1 << 0

// HeaderSize is the fixed byte length of WriteHeader's output.
const HeaderSize = 16

// WriteHeader emits the fixed 16-byte container header.
func WriteHeader(w io.Writer, recordCount uint32, flags uint32) error {
	buf := make([]byte, HeaderSize)
	copy(buf[0:4], Magic)
	buf[4] = Version
	binary.BigEndian.PutUint32(buf[8:12], recordCount)
	binary.BigEndian.PutUint32(buf[12:16], flags)
	_, err := w.Write(buf)
	return err
}

func checksum(kind uint16, flags uint16, seq uint32, ts int64, meta, payload []byte) [32]byte {
	h := sha256.New()
	var b [8]byte
	binary.BigEndian.PutUint16(b[:2], kind)
	h.Write(b[:2])
	binary.BigEndian.PutUint16(b[:2], flags)
	h.Write(b[:2])
	binary.BigEndian.PutUint32(b[:4], seq)
	h.Write(b[:4])
	binary.BigEndian.PutUint64(b[:8], uint64(ts))
	h.Write(b[:8])
	binary.BigEndian.PutUint32(b[:4], uint32(len(meta)))
	h.Write(b[:4])
	h.Write(meta)
	binary.BigEndian.PutUint32(b[:4], uint32(len(payload)))
	h.Write(b[:4])
	h.Write(payload)
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// RecordSize returns the on-disk size of one record with the given lengths.
func RecordSize(metaLen, payloadLen int) int {
	return 4 + 2 + 2 + 4 + 8 + 4 + metaLen + 4 + payloadLen + sha256.Size
}

// WriteRecord encodes one record. The checksum field is computed and written
// automatically; callers never supply it.
func WriteRecord(w io.Writer, kind uint16, flags uint16, seq uint32, ts int64, meta, payload []byte) ([32]byte, error) {
	sum := checksum(kind, flags, seq, ts, meta, payload)
	size := RecordSize(len(meta), len(payload))
	buf := make([]byte, 0, size)
	var b [8]byte
	binary.BigEndian.PutUint32(b[:4], uint32(size))
	buf = append(buf, b[:4]...)
	binary.BigEndian.PutUint16(b[:2], kind)
	buf = append(buf, b[:2]...)
	binary.BigEndian.PutUint16(b[:2], flags)
	buf = append(buf, b[:2]...)
	binary.BigEndian.PutUint32(b[:4], seq)
	buf = append(buf, b[:4]...)
	binary.BigEndian.PutUint64(b[:8], uint64(ts))
	buf = append(buf, b[:8]...)
	binary.BigEndian.PutUint32(b[:4], uint32(len(meta)))
	buf = append(buf, b[:4]...)
	buf = append(buf, meta...)
	binary.BigEndian.PutUint32(b[:4], uint32(len(payload)))
	buf = append(buf, b[:4]...)
	buf = append(buf, payload...)
	buf = append(buf, sum[:]...)
	_, err := w.Write(buf)
	return sum, err
}

// DecodeFrame parses canonical video metadata and builds a Frame.
func DecodeFrame(seq uint32, ts int64, flags uint16, meta, payload []byte, sum [32]byte, offset int64) (*Frame, error) {
	var vm VideoMeta
	if err := json.Unmarshal(meta, &vm); err != nil {
		return nil, NewError(ErrKind, "video metadata: %v", err).At(offset)
	}
	return &Frame{
		Seq: seq, TimestampNS: ts, Width: vm.Width, Height: vm.Height,
		ColorMeta: vm.Color, Keyframe: flags&FlagKeyframe != 0,
		Metadata: append([]byte(nil), meta...), Payload: payload,
		Checksum: sum, Offset: offset,
	}, nil
}

// DecodeAudio parses canonical audio metadata and builds an AudioBlock.
func DecodeAudio(seq uint32, ts int64, meta, payload []byte, sum [32]byte, offset int64) (*AudioBlock, error) {
	var am AudioMeta
	if err := json.Unmarshal(meta, &am); err != nil {
		return nil, NewError(ErrKind, "audio metadata: %v", err).At(offset)
	}
	return &AudioBlock{
		Seq: seq, TimestampNS: ts, SampleRate: am.SampleRate,
		Channels: am.Channels, Samples: am.Samples,
		Metadata: append([]byte(nil), meta...), Payload: payload,
		Checksum: sum, Offset: offset,
	}, nil
}

// VerifyChecksum recomputes the record digest and reports a mismatch.
func VerifyChecksum(kind uint16, flags uint16, seq uint32, ts int64, meta, payload []byte, want [32]byte) error {
	got := checksum(kind, flags, seq, ts, meta, payload)
	if got != want {
		return NewError(ErrChecksum, "want %x got %x", want[:8], got[:8]).WithRecord(kind, seq)
	}
	return nil
}

// EnsureIOError normalises short reads into truncation errors.
func EnsureIOError(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return NewError(ErrTruncated, "unexpected end of container")
	}
	return err
}
