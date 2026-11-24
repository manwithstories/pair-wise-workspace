package mp4

import (
	"encoding/binary"

	gomp4 "github.com/abema/go-mp4"
)

func writeSTSD(w *gomp4.Writer) error {
	if err := startBox(w, "stsd"); err != nil {
		return err
	}
	if err := writeFullHeader(w, 0, 0); err != nil {
		return err
	}
	if err := writeBE(w, uint32(1)); err != nil { // one sample description
		return err
	}

	// Uncompressed big-endian LPCM ("lpcm"), stereo, 16-bit, 48 kHz.
	if err := startBox(w, "lpcm"); err != nil {
		return err
	}
	if _, err := w.Write(make([]byte, 6)); err != nil { // reserved SampleEntry
		return err
	}
	if err := writeBE(w, uint16(1)); err != nil { // data reference index
		return err
	}
	if _, err := w.Write(make([]byte, 8)); err != nil { // reserved AudioSampleEntry
		return err
	}
	if err := writeBE(w, uint16(2)); err != nil { // channels
		return err
	}
	if err := writeBE(w, uint16(16)); err != nil { // sample size
		return err
	}
	if err := writeBE(w, uint16(0)); err != nil { // pre-defined
		return err
	}
	if err := writeBE(w, uint16(0)); err != nil { // reserved
		return err
	}
	if err := writeBE(w, uint32(48000<<16)); err != nil { // 16.16 sample rate
		return err
	}
	return endBox(w) // lpcm
}

func writeEmptyFullBox(w *gomp4.Writer, typ string, payloadWords ...uint32) error {
	if err := startBox(w, typ); err != nil {
		return err
	}
	if err := writeFullHeader(w, 0, 0); err != nil {
		return err
	}
	for _, v := range payloadWords {
		if err := writeBE(w, v); err != nil {
			return err
		}
	}
	return endBox(w)
}

func writeSTBL(w *gomp4.Writer) error {
	if err := startBox(w, "stbl"); err != nil {
		return err
	}
	if err := writeSTSD(w); err != nil {
		return err
	}
	// Fragments carry sample timing/sizes; the init tables are intentionally
	// empty but structurally present.
	if err := writeEmptyFullBox(w, "stts", 0); err != nil { // entry count
		return err
	}
	if err := writeEmptyFullBox(w, "stsc", 0); err != nil {
		return err
	}
	if err := writeEmptyFullBox(w, "stsz", 0, 0); err != nil { // size=0,count=0
		return err
	}
	if err := writeEmptyFullBox(w, "stco", 0); err != nil {
		return err
	}
	return endBox(w)
}

func writeMINF(w *gomp4.Writer) error {
	if err := startBox(w, "minf"); err != nil {
		return err
	}
	if err := writeSMHD(w); err != nil {
		return err
	}
	if err := writeDREF(w); err != nil {
		return err
	}
	if err := writeSTBL(w); err != nil {
		return err
	}
	return endBox(w)
}

func writeMDIA(w *gomp4.Writer, duration uint32) error {
	if err := startBox(w, "mdia"); err != nil {
		return err
	}
	if err := writeMDHD(w, duration); err != nil {
		return err
	}
	if err := writeHDLR(w); err != nil {
		return err
	}
	if err := writeMINF(w); err != nil {
		return err
	}
	return endBox(w)
}

func writeTRAK(w *gomp4.Writer, duration uint32) error {
	if err := startBox(w, "trak"); err != nil {
		return err
	}
	if err := writeTKHD(w, duration); err != nil {
		return err
	}
	if err := writeMDIA(w, duration); err != nil {
		return err
	}
	return endBox(w)
}

func writeMVEX(w *gomp4.Writer) error {
	if err := startBox(w, "mvex"); err != nil {
		return err
	}
	if err := startBox(w, "trex"); err != nil {
		return err
	}
	if err := writeFullHeader(w, 0, 0); err != nil {
		return err
	}
	words := []uint32{
		1, // track ID
		1, // default sample description index
		0, // default sample duration
		0, // default sample size
		0, // default sample flags
	}
	for _, v := range words {
		if err := writeBE(w, v); err != nil {
			return err
		}
	}
	if err := endBox(w); err != nil { // trex
		return err
	}
	return endBox(w) // mvex
}

func writeMOOV(w *gomp4.Writer, duration uint32) error {
	if err := startBox(w, "moov"); err != nil {
		return err
	}
	if err := writeMVHD(w, duration); err != nil {
		return err
	}
	if err := writeTRAK(w, duration); err != nil {
		return err
	}
	if err := writeMVEX(w); err != nil {
		return err
	}
	return endBox(w)
}

// initSizeHint is not required by the writer, but keeps encoding/binary used
// if future fixed offsets need big-endian helpers.
var _ = binary.BigEndian
