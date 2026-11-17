// Package garc 实现自研 GARC 媒体容器的底层解析：带边界检查的区间读取、
// IEEE CRC32 校验、LEB128 变长整数与结构化解析错误。
package garc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// MaxFileSize 单文件默认大小上限：512 MiB。
const MaxFileSize = 512 * 1024 * 1024

// Error 是所有容器层结构化错误的统一形态，携带代码、偏移、轨道与恢复建议，
// 供上层 reporter 原样落盘审计。
type Error struct {
	Code    string `json:"code"`
	Offset  int64  `json:"offset,omitempty"`
	TrackID uint32 `json:"track_id,omitempty"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

func (e *Error) Error() string {
	if e.Offset > 0 {
		return fmt.Sprintf("%s at offset %d: %s", e.Code, e.Offset, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func newError(code string, off int64, msg, hint string) *Error {
	return &Error{Code: code, Offset: off, Message: msg, Hint: hint}
}

// NewError 供其它包构造与容器层同构的结构化错误。
func NewError(code string, off int64, msg string) *Error {
	return newError(code, off, msg, "")
}

// ErrUnexpectedEOF 供调用方做 errors.Is 判断（结构化细节仍在 Error 中）。
var ErrUnexpectedEOF = errors.New("garc: unexpected end of file")

// Reader 包装完整文件字节，所有读取均经过范围与溢出检查。
// 字节序由文件头声明，调用方在解析出 endianness 后通过 SetOrder 注入。
type Reader struct {
	data  []byte
	order binary.ByteOrder
}

// NewReader 校验文件大小上限并构造 Reader。
func NewReader(data []byte) (*Reader, error) {
	if len(data) > MaxFileSize {
		return nil, newError("FILE_TOO_LARGE", int64(len(data)),
			fmt.Sprintf("file size %d exceeds limit %d", len(data), MaxFileSize),
			"split or re-export the asset; the hard limit is 512 MiB")
	}
	return &Reader{data: data}, nil
}

// SetOrder 注入文件头声明的字节序。
func (r *Reader) SetOrder(o binary.ByteOrder) { r.order = o }

// Len 返回文件实际字节数。
func (r *Reader) Len() int { return len(r.data) }

// CheckRange 验证 [off, off+length) 完全落在文件内，并拒绝整数溢出。
func (r *Reader) CheckRange(off, length int64) error {
	if off < 0 || length < 0 {
		return newError("OUT_OF_BOUNDS", off, "negative offset or length",
			"re-export the file; negative ranges are impossible")
	}
	end := off + length
	if end < off || end > int64(len(r.data)) {
		return newError("OUT_OF_BOUNDS", off,
			fmt.Sprintf("range [%d,%d) exceeds file length %d", off, end, len(r.data)),
			"the declared length runs past EOF; re-export or reject the file")
	}
	return nil
}

// Bytes 返回区间的只读切片（不复制，调用方不得修改）。
func (r *Reader) Bytes(off, length int64) ([]byte, error) {
	if err := r.CheckRange(off, length); err != nil {
		return nil, err
	}
	return r.data[off : off+length], nil
}

// U8 / U16 / U32 按显式字节序读取定长无符号整数。
func (r *Reader) U8(off int64) (byte, error) {
	if err := r.CheckRange(off, 1); err != nil {
		return 0, err
	}
	return r.data[off], nil
}

func (r *Reader) U16(off int64) (uint16, error) {
	if err := r.CheckRange(off, 2); err != nil {
		return 0, err
	}
	return r.order.Uint16(r.data[off : off+2]), nil
}

func (r *Reader) U32(off int64) (uint32, error) {
	if err := r.CheckRange(off, 4); err != nil {
		return 0, err
	}
	return r.order.Uint32(r.data[off : off+4]), nil
}

// Varint 读取无符号 LEB128。限制为 uint32 域（最多 5 个有效字节），
// 超长或截断分别返回 VARINT_OVERFLOW / TRUNCATED_RECORD。
func (r *Reader) Varint(off int64) (value uint64, n int, err error) {
	var shift uint
	for i := 0; i < 5; i++ {
		b, e := r.U8(off + int64(i))
		if e != nil {
			return 0, i, newError("TRUNCATED_RECORD", off+int64(i),
				"varint runs past end of file", "re-export the file")
		}
		n++
		if i == 4 && b&0xF0 != 0 {
			return 0, n, newError("VARINT_OVERFLOW", off,
				"LEB128 varint exceeds uint32 domain",
				"reject the record; length encoding is corrupt")
		}
		value |= uint64(b&0x7F) << shift
		if b&0x80 == 0 {
			return value, n, nil
		}
		shift += 7
	}
	return 0, n, newError("VARINT_OVERFLOW", off,
		"LEB128 varint never terminates", "reject the record")
}

// CRC32 计算给定区间的 IEEE CRC32，区间非法时返回错误。
func (r *Reader) CRC32(off, length int64) (uint32, error) {
	b, err := r.Bytes(off, length)
	if err != nil {
		return 0, err
	}
	return crc32.ChecksumIEEE(b), nil
}

// CheckCRC 比对区间 CRC 与期望值，不符返回 CHECKSUM_ERROR。
func (r *Reader) CheckCRC(off, length int64, want uint32) error {
	got, err := r.CRC32(off, length)
	if err != nil {
		return err
	}
	if got != want {
		return newError("CHECKSUM_ERROR", off,
			fmt.Sprintf("crc32 mismatch: want %08x got %08x", want, got),
			"the record is corrupt; reject and request re-export")
	}
	return nil
}

// CheckAddU32 执行 uint32 加法溢出检查。
func CheckAddU32(a, b uint32) (uint32, bool) {
	sum := uint64(a) + uint64(b)
	if sum > 0xFFFFFFFF {
		return 0, false
	}
	return uint32(sum), true
}
