package mp4

import (
	"encoding/binary"
	"io"

	gomp4 "github.com/abema/go-mp4"
)

func startBox(w *gomp4.Writer, boxType string) error {
	_, err := w.StartBox(&gomp4.BoxInfo{Type: gomp4.StrToBoxType(boxType)})
	return err
}

func endBox(w *gomp4.Writer) error {
	_, err := w.EndBox()
	return err
}

func writeFullHeader(w io.Writer, version byte, flags uint32) error {
	var b [4]byte
	b[0] = version
	b[1] = byte(flags >> 16)
	b[2] = byte(flags >> 8)
	b[3] = byte(flags)
	_, err := w.Write(b[:])
	return err
}

func writeBE(w io.Writer, v any) error {
	return binary.Write(w, binary.BigEndian, v)
}

func identityMatrix() []byte {
	m := make([]byte, 36)
	binary.BigEndian.PutUint32(m[0:4], 0x00010000)
	binary.BigEndian.PutUint32(m[16:20], 0x00010000)
	binary.BigEndian.PutUint32(m[32:36], 0x40000000)
	return m
}

func writeFTYP(w *gomp4.Writer) error {
	if err := startBox(w, "ftyp"); err != nil {
		return err
	}
	if _, err := w.Write([]byte("isom")); err != nil { // major brand
		return err
	}
	if err := writeBE(w, uint32(0x00000200)); err != nil { // minor version
		return err
	}
	if _, err := w.Write([]byte("isomiso2mp41dash")); err != nil {
		return err
	}
	return endBox(w)
}

func writeMVHD(w *gomp4.Writer, duration uint32) error {
	if err := startBox(w, "mvhd"); err != nil {
		return err
	}
	if err := writeFullHeader(w, 0, 0); err != nil {
		return err
	}
	for _, v := range []uint32{0, 0, 48000, duration} { // creation/modification/timescale/duration
		if err := writeBE(w, v); err != nil {
			return err
		}
	}
	if err := writeBE(w, uint32(0x00010000)); err != nil { // rate 1.0
		return err
	}
	if err := writeBE(w, uint16(0x0100)); err != nil { // volume 1.0
		return err
	}
	if _, err := w.Write(make([]byte, 10)); err != nil { // reserved
		return err
	}
	if _, err := w.Write(identityMatrix()); err != nil {
		return err
	}
	if _, err := w.Write(make([]byte, 24)); err != nil { // pre-defined
		return err
	}
	if err := writeBE(w, uint32(2)); err != nil { // next track ID
		return err
	}
	return endBox(w)
}

func writeTKHD(w *gomp4.Writer, duration uint32) error {
	if err := startBox(w, "tkhd"); err != nil {
		return err
	}
	if err := writeFullHeader(w, 0, 0x000003); err != nil { // enabled + in movie
		return err
	}
	for _, v := range []uint32{0, 0, 1, 0, duration} {
		if err := writeBE(w, v); err != nil {
			return err
		}
	}
	if _, err := w.Write(make([]byte, 8)); err != nil { // reserved
		return err
	}
	for _, v := range []uint16{0, 0, 0x0100, 0} { // layer, alternate, volume, reserved
		if err := writeBE(w, v); err != nil {
			return err
		}
	}
	if _, err := w.Write(identityMatrix()); err != nil {
		return err
	}
	if _, err := w.Write(make([]byte, 8)); err != nil { // zero width/height for audio
		return err
	}
	return endBox(w)
}

func writeMDHD(w *gomp4.Writer, duration uint32) error {
	if err := startBox(w, "mdhd"); err != nil {
		return err
	}
	if err := writeFullHeader(w, 0, 0); err != nil {
		return err
	}
	for _, v := range []uint32{0, 0, 48000, duration} {
		if err := writeBE(w, v); err != nil {
			return err
		}
	}
	if err := writeBE(w, uint16(0x55c4)); err != nil { // language: und
		return err
	}
	if err := writeBE(w, uint16(0)); err != nil { // pre-defined
		return err
	}
	return endBox(w)
}

func writeHDLR(w *gomp4.Writer) error {
	if err := startBox(w, "hdlr"); err != nil {
		return err
	}
	if err := writeFullHeader(w, 0, 0); err != nil {
		return err
	}
	if err := writeBE(w, uint32(0)); err != nil { // pre-defined
		return err
	}
	if _, err := w.Write([]byte("soun")); err != nil {
		return err
	}
	if _, err := w.Write(make([]byte, 12)); err != nil { // reserved
		return err
	}
	if _, err := w.Write([]byte("mediad-sound-handler\x00")); err != nil {
		return err
	}
	return endBox(w)
}

func writeSMHD(w *gomp4.Writer) error {
	if err := startBox(w, "smhd"); err != nil {
		return err
	}
	if err := writeFullHeader(w, 0, 0); err != nil {
		return err
	}
	if err := writeBE(w, uint16(0)); err != nil { // balance
		return err
	}
	if err := writeBE(w, uint16(0)); err != nil { // reserved
		return err
	}
	return endBox(w)
}

func writeDREF(w *gomp4.Writer) error {
	if err := startBox(w, "dinf"); err != nil {
		return err
	}
	if err := startBox(w, "dref"); err != nil {
		return err
	}
	if err := writeFullHeader(w, 0, 0); err != nil {
		return err
	}
	if err := writeBE(w, uint32(1)); err != nil { // entry count
		return err
	}
	if err := startBox(w, "url "); err != nil {
		return err
	}
	if err := writeFullHeader(w, 0, 1); err != nil { // self-contained flag
		return err
	}
	if err := endBox(w); err != nil {
		return err
	}
	if err := endBox(w); err != nil { // dref
		return err
	}
	return endBox(w) // dinf
}
