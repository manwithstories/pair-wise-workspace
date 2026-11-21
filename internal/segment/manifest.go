package segment

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"io"

	"github.com/abema/go-mp4"

	"archivecheck/internal/model"
)

// CanonicalEncode 把清单序列化为定长字段的规范化二进制表示。
// 不使用 map、不依赖 JSON 字段顺序：相同清单在任何机器上字节一致，
// 因此清单哈希具备跨运行可复核性。
func CanonicalEncode(m model.Manifest) []byte {
	var buf bytes.Buffer
	writeStr := func(s string) {
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], uint16(len(s)))
		buf.Write(b[:])
		buf.WriteString(s)
	}
	writeU32 := func(v uint32) {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], v)
		buf.Write(b[:])
	}
	writeI64 := func(v int64) {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(v))
		buf.Write(b[:])
	}
	writeStr(m.Rep)
	writeU32(uint32(len(m.Segments)))
	for _, s := range m.Segments {
		writeI64(int64(s.Index))
		writeStr(s.Rep)
		writeU32(s.FirstSeq)
		writeU32(s.LastSeq)
		writeI64(s.StartNS)
		writeI64(s.EndNS)
		buf.Write(s.PayloadSHA256)
		writeI64(s.ToleranceNS)
		if s.TailIncomplete {
			buf.WriteByte(1)
		} else {
			buf.WriteByte(0)
		}
	}
	writeU32(uint32(len(m.Gaps)))
	for _, g := range m.Gaps {
		writeU32(g.Start)
		writeU32(g.End)
	}
	writeI64(m.MediaSpan)
	return buf.Bytes()
}

// Hash 返回规范化清单的 SHA-256 十六进制摘要。
func Hash(m model.Manifest) [32]byte {
	return sha256.Sum256(CanonicalEncode(m))
}

// WriteMP4Association 用 go-mp4 写出最小 ftyp+moov 容器，
// 在 udta 中嵌入清单摘要，建立分片与清单的封装级关联。
// 该容器仅用于归档关联，不承载解码所需的真实媒体轨道。
func WriteMP4Association(w io.WriteSeeker, m model.Manifest) error {
	mw := mp4.NewWriter(w)
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(mw, &mp4.Ftyp{
		MajorBrand:   [4]byte{'a', 'r', 'c', 'h'},
		MinorVersion: 1,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'a', 'r', 'c', 'h'}},
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
		},
	}, mp4.Context{}); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil {
		return err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		return err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeUdta()}); err != nil {
		return err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.StrToBoxType("mhsd")}); err != nil {
		return err
	}
	digest := Hash(m)
	if _, err := w.Write(digest[:]); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil { // mhsd
		return err
	}
	if _, err := mw.EndBox(); err != nil { // udta
		return err
	}
	if _, err := mw.EndBox(); err != nil { // moov
		return err
	}
	return nil
}
