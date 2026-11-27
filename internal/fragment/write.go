package fragment

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"os"
)

// 写盒子辅助。
func putBox(buf *bytes.Buffer, typ string, payload []byte) {
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)+8))
	copy(hdr[4:], typ)
	buf.Write(hdr[:])
	buf.Write(payload)
}

func u32(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}

// WriteInit 写初始化段：ftyp + moov(rinit)。
func WriteInit(path string, info *InitInfo) error {
	var p bytes.Buffer
	p.WriteString(pad4(info.Codec))
	p.Write(u32(uint32(info.Timescale)))
	var tmp [2]byte
	binary.BigEndian.PutUint16(tmp[:], uint16(info.Width))
	p.Write(tmp[:])
	binary.BigEndian.PutUint16(tmp[:], uint16(info.Height))
	p.Write(tmp[:])
	p.Write(u32(uint32(info.SampleRate)))
	binary.BigEndian.PutUint16(tmp[:], uint16(info.Channels))
	p.Write(tmp[:])
	p.Write(u32(uint32(info.FrameDuration)))
	p.Write(u32(0)) // 保留

	var moov bytes.Buffer
	putBox(&moov, "rinit", p.Bytes())

	var out bytes.Buffer
	putBox(&out, "ftyp", []byte("isom\x00\x00\x00\x00"))
	putBox(&out, "moov", moov.Bytes())
	return os.WriteFile(path, out.Bytes(), 0o644)
}

func pad4(s string) string {
	for len(s) < 4 {
		s += " "
	}
	return s[:4]
}

// WriteSegment 写分片：sidx + moof(tfdt,trun) + mdat。
// dropIndex 表示 sidx 少写的条目数（索引缺项夹具）。
func WriteSegment(path string, baseTime int64, payloads [][]byte, duration uint32, dropIndex int) ([]byte, error) {
	var sidx bytes.Buffer
	nIdx := len(payloads) - dropIndex
	if nIdx < 0 {
		nIdx = 0
	}
	sidx.Write(u32(uint32(nIdx)))
	for i := 0; i < nIdx; i++ {
		sidx.Write(u32(uint32(len(payloads[i]))))
		sidx.Write(u32(duration))
	}

	var tfdt bytes.Buffer
	var t8 [8]byte
	binary.BigEndian.PutUint64(t8[:], uint64(baseTime))
	tfdt.Write(t8[:])

	var trun bytes.Buffer
	trun.Write(u32(uint32(len(payloads))))
	for _, pl := range payloads {
		trun.Write(u32(duration))
		trun.Write(u32(uint32(len(pl))))
		trun.Write(u32(crc32.ChecksumIEEE(pl)))
	}

	var moof bytes.Buffer
	putBox(&moof, "tfdt", tfdt.Bytes())
	putBox(&moof, "trun", trun.Bytes())

	var mdat bytes.Buffer
	for _, pl := range payloads {
		mdat.Write(pl)
	}

	var out bytes.Buffer
	putBox(&out, "sidx", sidx.Bytes())
	putBox(&out, "moof", moof.Bytes())
	putBox(&out, "mdat", mdat.Bytes())
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
