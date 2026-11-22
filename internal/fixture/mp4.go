package fixture

import (
	"encoding/binary"
	"io"
	"os"

	"mediaslice/media"
)

// WriteMP4 serializes the plan as a minimal valid MP4: ftyp, moov (mvhd +
// two traks carrying mdhd/tkhd/hdlr/edts/elst/minf/stbl) and one mdat whose
// chunk offsets point at the video then audio payloads. Two passes are used
// so the stco boxes can carry exact mdat-relative payload offsets.
func WriteMP4(path string, p *Plan) error {
	// Pass 1: measure ftyp+moov size to locate the mdat payload origin.
	probe := newCountWriter()
	bw0 := NewBoxWriter(probe)
	writeFtyp(bw0)
	must(writeMoov(bw0, p))
	mdatHeader := int64(8)
	p.videoChunkOffset = uint32(probe.size + mdatHeader)
	p.audioChunkOffset = p.videoChunkOffset + uint32(len(p.Video)*p.Spec.VideoSampleSize)

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	bw := NewBoxWriter(f)
	writeFtyp(bw)
	if err := writeMoov(bw, p); err != nil {
		return err
	}
	must(bw.Start("mdat"))
	buf := make([]byte, 256)
	writeSamples := func(n, size, seed int) error {
		for i := 0; i < n; i++ {
			for j := 0; j < size; j++ {
				buf[j] = byte((i*31 + j*7 + seed) & 0xff)
			}
			if err := writeN(bw, buf[:size]); err != nil {
				return err
			}
		}
		return nil
	}
	must(writeSamples(len(p.Video), p.Spec.VideoSampleSize, 11))
	must(writeSamples(len(p.Audio), p.Spec.AudioSampleSize, 23))
	must(bw.End())
	return nil
}

// countWriter measures serialized bytes without retaining payload content,
// keeping the two-pass build memory footprint independent of mdat size. It
// emulates a real write cursor so box-size rewinds and re-seeks land where
// they would in the output file.
type countWriter struct {
	pos  int64
	size int64
}

func newCountWriter() *countWriter { return &countWriter{} }

func (c *countWriter) Write(p []byte) (int, error) {
	c.pos += int64(len(p))
	if c.pos > c.size {
		c.size = c.pos
	}
	return len(p), nil
}
func (c *countWriter) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		c.pos = offset
	case io.SeekCurrent:
		c.pos += offset
	case io.SeekEnd:
		c.pos = c.size + offset
	}
	return c.pos, nil
}

func writeN(w io.Writer, p []byte) error {
	_, err := w.Write(p)
	return err
}

func writeFtyp(bw *BoxWriter) {
	must(bw.Start("ftyp"))
	buf := make([]byte, 0, 16)
	buf = append(buf, []byte("isom")...)
	buf = U32(buf, 0x200)
	buf = append(buf, []byte("isom")...)
	buf = append(buf, []byte("iso2")...)
	buf = append(buf, []byte("mp41")...)
	_, _ = bw.Write(buf)
	must(bw.End())
}

// WriteRTPSidecar concatenates every RTP packet, each prefixed with a 16-bit
// length, so the scanner can stream them without an index.
func WriteRTPSidecar(path string, p *Plan) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	hdr := make([]byte, 2)
	n := len(p.RTPBuf) / RTPPacketSize
	for i := 0; i < n; i++ {
		binary.BigEndian.PutUint16(hdr, RTPPacketSize)
		if _, err := f.Write(hdr); err != nil {
			return err
		}
		if _, err := f.Write(p.RTPBuf[i*RTPPacketSize:(i+1)*RTPPacketSize]); err != nil {
			return err
		}
	}
	return nil
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func writeMoov(bw *BoxWriter, p *Plan) error {
	must(bw.Start("moov"))
	must(bw.Start("mvhd"))
	must(bw.FullBox(0, 0))
	buf := make([]byte, 0, 96)
	buf = U32(buf, 0) // creation
	buf = U32(buf, 0) // modification
	buf = U32(buf, media.ClockRate)
	buf = U32(buf, uint32(p.Spec.DurationTicks))
	buf = U32(buf, 0x00010000) // rate 1.0
	buf = append(buf, 0x01, 0x00)
	buf = append(buf, 0, 0)
	buf = append(buf, make([]byte, 8)...)
	buf = append(buf, identityMatrix()...)
	buf = append(buf, make([]byte, 24)...)
	buf = U32(buf, 3) // next track id
	_, _ = bw.Write(buf)
	must(bw.End())

	if err := writeVideoTrak(bw, p); err != nil {
		return err
	}
	if err := writeAudioTrak(bw, p); err != nil {
		return err
	}
	return bw.End()
}

func identityMatrix() []byte {
	m := make([]byte, 0, 36)
	for i := 0; i < 9; i++ {
		v := uint32(0)
		if i == 0 || i == 4 || i == 8 {
			v = 0x00010000
		}
		m = U32(m, v)
	}
	return m
}
