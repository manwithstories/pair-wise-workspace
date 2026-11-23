package fixture

import (
	"encoding/binary"
	"hash/fnv"
)

const (
	moovHeaderLen = 8
	tkhdLen       = 84
	mdhdLen       = 32
	hdlrLen       = 32 // fullbox(4) predefined(4) type(4) reserved(12) name(1) + hdr(8)
	vmhdLen       = 20
	smhdLen       = 16
	dinfLen       = 8 + 8 + 16
	stsdHeaderLen = 16
	videoEntryLen = 86
	audioEntryLen = 36
	sttsLen       = 8 + 8 + 8
	stscLen       = 8 + 4 + 12
	stszHeaderLen = 20 // fullbox + sample_size + count
	stcoHeaderLen = 16 // fullbox + count
	stssHeaderLen = 16 // fullbox + count
	stblHeaderLen = 8
	minfHeaderLen = 8
	mdiaHeaderLen = 8
	trakHeaderLen = 8
)

// samplePayload writes a self-describing synthetic sample.
func samplePayload(seed int64, source string, idx int, size int, sync bool) []byte {
	buf := make([]byte, size)
	copy(buf, "MSMP")
	flags := byte(0)
	if sync {
		flags = 1
	}
	buf[4] = flags
	binary.BigEndian.PutUint32(buf[5:9], uint32(size))
	binary.BigEndian.PutUint16(buf[9:11], uint16(idx&0xffff))
	buf[11] = byte(idx >> 16)
	buf[12] = byte(len(source))
	binary.BigEndian.PutUint16(buf[13:15], uint16(seed))
	buf[15] = payloadByte(seed, source, idx, 0)
	copy(buf[HeaderLen:], source) // name follows the fixed header
	// Deterministic body so PNG/audio content and hashes are stable. It
	// starts after the name so the scanner can read the source back.
	start := HeaderLen + len(source)
	for i := start; i < size; i += 64 {
		buf[i] = payloadByte(seed, source, idx, i)
	}
	return buf
}

func payloadByte(seed int64, source string, idx, off int) byte {
	h := fnv.New64a()
	var tmp [16]byte
	binary.BigEndian.PutUint64(tmp[:8], uint64(seed))
	binary.BigEndian.PutUint32(tmp[8:], uint32(idx))
	binary.BigEndian.PutUint32(tmp[12:], uint32(off))
	h.Write(tmp[:])
	h.Write([]byte(source))
	return byte(h.Sum64())
}

func isKeyframe(idx int, every int) bool { return every > 0 && idx%every == 0 }
