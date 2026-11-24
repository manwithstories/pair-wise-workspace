package mp4

import (
	"bytes"
	"encoding/binary"

	gomp4 "github.com/abema/go-mp4"

	"mediarig/internal/media"
)

func writeFtyp(mw *gomp4.Writer) error {
	if _, err := mw.StartBox(&gomp4.BoxInfo{Type: gomp4.BoxTypeFtyp()}); err != nil {
		return media.InternalWrap(media.StageMP4, err, "start ftyp")
	}
	var p bytes.Buffer
	p.WriteString("isom")
	binary.Write(&p, binary.BigEndian, uint32(0x00000200))
	p.WriteString("isom")
	p.WriteString("iso2")
	p.WriteString("mp41")
	if _, err := mw.Write(p.Bytes()); err != nil {
		return media.InternalWrap(media.StageMP4, err, "write ftyp payload")
	}
	if _, err := mw.EndBox(); err != nil {
		return media.InternalWrap(media.StageMP4, err, "end ftyp")
	}
	return nil
}

func writeFullBox(mw *gomp4.Writer, typ string, version byte, flags [3]byte, payload []byte) error {
	if _, err := mw.StartBox(&gomp4.BoxInfo{Type: gomp4.StrToBoxType(typ)}); err != nil {
		return media.InternalWrap(media.StageMP4, err, "start %s", typ)
	}
	head := [4]byte{version, flags[0], flags[1], flags[2]}
	if _, err := mw.Write(head[:]); err != nil {
		return media.InternalWrap(media.StageMP4, err, "write %s full header", typ)
	}
	if len(payload) > 0 {
		if _, err := mw.Write(payload); err != nil {
			return media.InternalWrap(media.StageMP4, err, "write %s payload", typ)
		}
	}
	if _, err := mw.EndBox(); err != nil {
		return media.InternalWrap(media.StageMP4, err, "end %s", typ)
	}
	return nil
}

func writeContainer(mw *gomp4.Writer, typ string, fn func() error) error {
	if _, err := mw.StartBox(&gomp4.BoxInfo{Type: gomp4.StrToBoxType(typ)}); err != nil {
		return media.InternalWrap(media.StageMP4, err, "start %s", typ)
	}
	if err := fn(); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil {
		return media.InternalWrap(media.StageMP4, err, "end %s", typ)
	}
	return nil
}

func writeRawBox(mw *gomp4.Writer, typ string, payload []byte) error {
	if _, err := mw.StartBox(&gomp4.BoxInfo{Type: gomp4.StrToBoxType(typ)}); err != nil {
		return media.InternalWrap(media.StageMP4, err, "start %s", typ)
	}
	if _, err := mw.Write(payload); err != nil {
		return media.InternalWrap(media.StageMP4, err, "write %s", typ)
	}
	if _, err := mw.EndBox(); err != nil {
		return media.InternalWrap(media.StageMP4, err, "end %s", typ)
	}
	return nil
}

func be16(v uint16) []byte {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	return b[:]
}

func be32(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}
