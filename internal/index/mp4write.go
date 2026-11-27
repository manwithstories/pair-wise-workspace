package index

import (
	"bytes"
	"encoding/binary"
	"io"
)

// atomWriter is a minimal, size-patching MP4 atom builder shared by tests
// and the fixture generator. It deliberately supports only the subset of
// ISO/IEC 14496-12 needed for H.264/AAC remux fixtures.
type atomWriter struct {
	w io.Writer
}

func newAtomWriter(w io.Writer) *atomWriter { return &atomWriter{w: w} }

func putU16(b []byte, v uint16) { binary.BigEndian.PutUint16(b, v) }
func putU32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }

// box writes a length-prefixed atom containing payload.
func (a *atomWriter) box(typ string, payload []byte) error {
	hdr := make([]byte, 8)
	putU32(hdr, uint32(8+len(payload)))
	copy(hdr[4:], typ)
	if _, err := a.w.Write(hdr); err != nil {
		return err
	}
	_, err := a.w.Write(payload)
	return err
}

// full prefixes payload with a version+flags FullBox header.
func full(version byte, flags uint32, payload []byte) []byte {
	out := make([]byte, 4, 4+len(payload))
	out[0] = version
	out[1] = byte(flags >> 16)
	out[2] = byte(flags >> 8)
	out[3] = byte(flags)
	return append(out, payload...)
}

// container concatenates already-serialized child atoms.
func container(children ...[]byte) []byte {
	var buf bytes.Buffer
	for _, c := range children {
		buf.Write(c)
	}
	return buf.Bytes()
}

func rawBox(typ string, payload []byte) []byte {
	hdr := make([]byte, 8)
	putU32(hdr, uint32(8+len(payload)))
	copy(hdr[4:], typ)
	return append(hdr, payload...)
}

// ftyp builds a file-type atom.
func ftyp(major string) []byte {
	p := make([]byte, 0, 16)
	p = append(p, []byte(major)...)
	p = binary.BigEndian.AppendUint32(p, 0)
	p = append(p, []byte("mp42")...)
	p = append(p, []byte("isom")...)
	return rawBox("ftyp", p)
}

// mvhd builds a minimal movie header with the given timescale/duration.
func mvhd(timescale uint32, duration uint64) []byte {
	p := make([]byte, 0, 108)
	p = binary.BigEndian.AppendUint64(p, 0) // creation
	p = binary.BigEndian.AppendUint64(p, 0) // modification
	p = binary.BigEndian.AppendUint32(p, timescale)
	p = binary.BigEndian.AppendUint64(p, duration)
	p = binary.BigEndian.AppendUint32(p, 0x00010000) // rate 1.0
	p = append(p, 0x01, 0x00)                       // volume
	p = append(p, make([]byte, 10)...)             // reserved
	// identity matrix (9 * uint32)
	matrix := []uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
	for _, m := range matrix {
		p = binary.BigEndian.AppendUint32(p, m)
	}
	p = append(p, make([]byte, 24)...) // pre-defined
	p = binary.BigEndian.AppendUint32(p, 2) // next track ID
	return rawBox("mvhd", full(1, 0, p))
}

// tkhd builds a version-1 track header.
func tkhd(trackID uint32, duration uint64, video bool) []byte {
	p := make([]byte, 0, 92)
	p = binary.BigEndian.AppendUint64(p, 0)
	p = binary.BigEndian.AppendUint64(p, 0)
	p = binary.BigEndian.AppendUint32(p, trackID)
	p = binary.BigEndian.AppendUint32(p, 0) // reserved
	p = binary.BigEndian.AppendUint64(p, duration)
	p = append(p, make([]byte, 8)...)      // reserved
	p = append(p, 0, 0)                    // layer/alt-group
	p = append(p, func() byte { if video { return 0 }; return 0x01 }(), 0)
	p = append(p, make([]byte, 2)...)      // reserved
	matrix := []uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
	for _, m := range matrix {
		p = binary.BigEndian.AppendUint32(p, m)
	}
	if video {
		p = binary.BigEndian.AppendUint32(p, 320<<16) // width 16.16
		p = binary.BigEndian.AppendUint32(p, 240<<16) // height
	} else {
		p = binary.BigEndian.AppendUint32(p, 0)
		p = binary.BigEndian.AppendUint32(p, 0)
	}
	return rawBox("tkhd", full(1, 3, p)) // flags=3 enabled+in-movie
}

// mdhd builds a version-1 media header.
func mdhd(timescale uint32, duration uint64) []byte {
	p := make([]byte, 0, 32)
	p = binary.BigEndian.AppendUint64(p, 0)
	p = binary.BigEndian.AppendUint64(p, 0)
	p = binary.BigEndian.AppendUint32(p, timescale)
	p = binary.BigEndian.AppendUint64(p, duration)
	p = append(p, 0x55, 0xC4) // language 'und'
	p = binary.BigEndian.AppendUint16(p, 0)
	return rawBox("mdhd", full(1, 0, p))
}

func hdlr(handler string, name string) []byte {
	p := make([]byte, 0, 33+len(name))
	p = binary.BigEndian.AppendUint32(p, 0) // pre-defined
	p = append(p, []byte(handler)...)
	p = append(p, make([]byte, 12)...) // reserved
	p = append(p, []byte(name)...)
	p = append(p, 0) // null terminator
	return rawBox("hdlr", full(0, 0, p))
}
