package media

import (
	"crypto/sha256"
	"encoding/binary"
	"hash/crc32"
)

// HeaderMagic 位于记录头偏移 36 的 4 字节魔数 "MF01"，用于识别错位记录。
var HeaderMagic = [4]byte{'M', 'F', '0', '1'}

// castagnoli 是 CRC32C（Castagnoli）多项式表，macOS 与 Linux 均可用软件实现。
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// CRC32C 计算 b 的 CRC32C 校验和。
func CRC32C(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }

// SumSHA256 返回载荷的 SHA-256 摘要。
func SumSHA256(payload []byte) [32]byte { return sha256.Sum256(payload) }

// PutHeader 将 42 字节记录头写入 buf（调用方保证 len>=HeaderLen）。
func PutHeader(buf []byte, f *Frame, payloadLen uint32) {
	buf[0] = f.Version
	buf[1] = f.Codec
	binary.LittleEndian.PutUint16(buf[2:4], f.CameraID)
	binary.LittleEndian.PutUint32(buf[4:8], f.Width)
	binary.LittleEndian.PutUint32(buf[8:12], f.Height)
	binary.LittleEndian.PutUint64(buf[12:20], f.Sequence)
	binary.LittleEndian.PutUint64(buf[20:28], uint64(f.EventNS))
	binary.LittleEndian.PutUint32(buf[28:32], payloadLen)
	binary.LittleEndian.PutUint32(buf[32:36], 0) // flags，保留必须为 0
	copy(buf[36:40], HeaderMagic[:])
	binary.LittleEndian.PutUint16(buf[40:42], 0) // reserved
}

// ParseHeader 从 42 字节缓冲解析记录头，校验魔数与版本。
// 返回的 Frame 不含载荷与尾部校验。
func ParseHeader(buf []byte) (*Frame, error) {
	if len(buf) < HeaderLen {
		return nil, NewFault(StageHeader, 0, 0, 0, "short header: %d bytes", len(buf))
	}
	if buf[0] != WireVersion1 {
		return nil, NewFault(StageVersion, 0, 0, 0, "unknown wire version %d", buf[0])
	}
	var magic [4]byte
	copy(magic[:], buf[36:40])
	if magic != HeaderMagic {
		return nil, NewFault(StageHeader, 0, 0, 0, "bad header magic %q", magic[:])
	}
	f := &Frame{
		Version:  buf[0],
		Codec:    buf[1],
		CameraID: binary.LittleEndian.Uint16(buf[2:4]),
		Width:    binary.LittleEndian.Uint32(buf[4:8]),
		Height:   binary.LittleEndian.Uint32(buf[8:12]),
		Sequence: binary.LittleEndian.Uint64(buf[12:20]),
		EventNS:  TimeNS(int64(binary.LittleEndian.Uint64(buf[20:28]))),
	}
	return f, nil
}

// PayloadLen 读取记录头中声明的载荷长度。
func PayloadLen(buf []byte) uint32 {
	return binary.LittleEndian.Uint32(buf[28:32])
}

// EncodeRecord 将帧编码为完整线格式记录（头 + 载荷 + SHA256 + CRC32C）。
// 摘要与校验和按载荷/头+载荷重新计算，保证写出的记录自洽。
func EncodeRecord(f *Frame) []byte {
	out := make([]byte, f.RecordLen())
	PutHeader(out[:HeaderLen], f, uint32(len(f.Payload)))
	copy(out[HeaderLen:], f.Payload)
	off := HeaderLen + len(f.Payload)
	sum := SumSHA256(f.Payload)
	copy(out[off:off+32], sum[:])
	// CRC32C 覆盖头与载荷（不含自身与 SHA 尾部）。
	crc := CRC32C(out[:off])
	binary.LittleEndian.PutUint32(out[off+32:off+36], crc)
	return out
}
