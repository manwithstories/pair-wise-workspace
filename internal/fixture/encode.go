package fixture

import "encoding/binary"

// klvAppend appends one KLV triplet: 16-byte key, BER long-form length
// (0x84 + 4-byte big-endian), then the value.
func klvAppend(dst []byte, key [16]byte, val []byte) []byte {
	dst = append(dst, key[:]...)
	dst = append(dst, 0x84, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(dst[len(dst)-4:], uint32(len(val)))
	return append(dst, val...)
}

// boxAppend appends one MP4 box: 4-byte size (header included), 4-byte
// type, then the payload.
func boxAppend(dst []byte, typ string, body []byte) []byte {
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(8+len(body)))
	copy(hdr[4:8], typ)
	dst = append(dst, hdr[:]...)
	return append(dst, body...)
}

// KLV keys, mirrored from the ingest package (fixture is the writer side
// of the same simplified format).
var (
	keyCDCI   = [16]byte{0x06, 0x0E, 0x2B, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0D, 0x01, 0x02, 0x01, 0x01, 0x05, 0x01, 0x00}
	keyAnchor = [16]byte{0x06, 0x0E, 0x2B, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0D, 0x01, 0x02, 0x01, 0x01, 0x05, 0x02, 0x00}
	keyFrame  = [16]byte{0x06, 0x0E, 0x2B, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0D, 0x01, 0x02, 0x01, 0x01, 0x05, 0x03, 0x00}
)

func cdciValue(w, h int, primaries, transfer, matrix, rotation byte) []byte {
	v := make([]byte, 8)
	binary.BigEndian.PutUint16(v[0:2], uint16(w))
	binary.BigEndian.PutUint16(v[2:4], uint16(h))
	v[4], v[5], v[6], v[7] = primaries, transfer, matrix, rotation
	return v
}

func anchorValue(anchors [][2]int64) []byte {
	v := []byte{byte(len(anchors))}
	for _, a := range anchors {
		v = binary.BigEndian.AppendUint64(v, uint64(a[0]))
		v = binary.BigEndian.AppendUint64(v, uint64(a[1]))
	}
	return v
}

func frameValue(pts33 int64, durTicks uint32, jpeg []byte) []byte {
	v := make([]byte, 9)
	binary.BigEndian.PutUint32(v[0:4], uint32(pts33>>8))
	v[4] = byte(pts33)
	binary.BigEndian.PutUint32(v[5:9], durTicks)
	return append(v, jpeg...)
}

func tkhdMatrix(vals [9]int32) []byte {
	v := make([]byte, 36)
	for i, x := range vals {
		binary.BigEndian.PutUint32(v[i*4:], uint32(x))
	}
	return v
}

func clapValue(x, y, w, h int) []byte {
	v := make([]byte, 16)
	binary.BigEndian.PutUint32(v[0:4], uint32(int32(x)))
	binary.BigEndian.PutUint32(v[4:8], uint32(int32(y)))
	binary.BigEndian.PutUint32(v[8:12], uint32(int32(w)))
	binary.BigEndian.PutUint32(v[12:16], uint32(int32(h)))
	return v
}

func apktValue(localPTS int64, samples uint32, pcm []byte) []byte {
	v := make([]byte, 12)
	binary.BigEndian.PutUint64(v[0:8], uint64(localPTS))
	binary.BigEndian.PutUint32(v[8:12], samples)
	return append(v, pcm...)
}

func par1Value(baseSeq int64, pts []int64, samples, lengths []int, xor []byte) []byte {
	v := make([]byte, 8)
	binary.BigEndian.PutUint32(v[0:4], uint32(baseSeq))
	binary.BigEndian.PutUint32(v[4:8], uint32(len(pts)))
	for i := range pts {
		v = binary.BigEndian.AppendUint64(v, uint64(pts[i]))
		v = binary.BigEndian.AppendUint32(v, uint32(samples[i]))
		v = binary.BigEndian.AppendUint32(v, uint32(lengths[i]))
	}
	return append(v, xor...)
}
