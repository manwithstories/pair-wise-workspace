package ingest

import (
	"crypto/sha256"
	"encoding/binary"
	"io"
	"os"

	"avpipe/internal/media"
)

// Container version and magic. The format is a fixed 32-byte header followed
// by self-delimiting records, all little-endian.
const (
	Magic        = "AVP1"
	HeaderSize   = 32
	RecordHdrLen = 22 // kind(1) + parityOf(1) + seq(4) + block(4) + pts(8) + payloadLen(4)
	ShaLen       = sha256.Size
)

// Header mirrors the on-disk container header and the source Manifest.
type Header struct {
	Frames    uint32
	FPS       uint32
	Width     uint16
	Height    uint16
	AudioRate uint32
	Packets   uint32
}

// WriteContainer serialises packets to path in the avpipe binary format.
// SHA-256 fields present on each packet are written as-is; callers that want
// checksum-failure fixtures deliberately pass a mismatched digest.
func WriteContainer(path string, h Header, packets []media.Packet) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	hdr := make([]byte, HeaderSize)
	copy(hdr[0:4], Magic)
	binary.LittleEndian.PutUint16(hdr[4:6], 1)
	binary.LittleEndian.PutUint32(hdr[8:12], h.Frames)
	binary.LittleEndian.PutUint32(hdr[12:16], h.FPS)
	binary.LittleEndian.PutUint16(hdr[16:18], h.Width)
	binary.LittleEndian.PutUint16(hdr[18:20], h.Height)
	binary.LittleEndian.PutUint32(hdr[20:24], h.AudioRate)
	binary.LittleEndian.PutUint32(hdr[24:28], uint32(len(packets)))
	if _, err := f.Write(hdr); err != nil {
		return err
	}
	buf := make([]byte, RecordHdrLen)
	for _, p := range packets {
		if len(p.Payload) > MaxPacketSize {
			return media.NewError(media.ClassValidation, "ingest", "packet exceeds size limit", p.PTS, false)
		}
		buf[0] = byte(p.Kind)
		buf[1] = byte(p.ParityOf)
		binary.LittleEndian.PutUint32(buf[2:6], p.Seq)
		binary.LittleEndian.PutUint32(buf[6:10], p.BlockID)
		binary.LittleEndian.PutUint64(buf[10:18], p.PTS)
		binary.LittleEndian.PutUint32(buf[18:22], uint32(len(p.Payload)))
		if _, err := f.Write(buf); err != nil {
			return err
		}
		digest := p.SHA256
		if len(digest) != ShaLen {
			d := sha256.Sum256(p.Payload)
			digest = d[:]
		}
		if _, err := f.Write(digest); err != nil {
			return err
		}
		if _, err := f.Write(p.Payload); err != nil {
			return err
		}
	}
	return f.Sync()
}

// PacketReader streams records from a container and verifies each digest.
type PacketReader struct {
	file   *os.File
	header Header
	read   uint32
	path   string
}

// OpenPacketReader validates the header and returns a streaming reader plus
// the Manifest describing the source.
func OpenPacketReader(path string) (*PacketReader, *Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, media.NewError(media.ClassValidation, "ingest", "cannot open source: "+err.Error(), 0, false)
	}
	info, _ := f.Stat()
	r := &PacketReader{file: f, path: path}
	hdr := make([]byte, HeaderSize)
	if _, err := io.ReadFull(f, hdr); err != nil {
		f.Close()
		return nil, nil, media.NewError(media.ClassValidation, "ingest", "short container header", 0, false)
	}
	if string(hdr[0:4]) != Magic {
		f.Close()
		return nil, nil, media.NewError(media.ClassValidation, "ingest", "bad container magic", 0, false)
	}
	if binary.LittleEndian.Uint16(hdr[4:6]) != 1 {
		f.Close()
		return nil, nil, media.NewError(media.ClassValidation, "ingest", "unsupported container version", 0, false)
	}
	r.header = Header{
		Frames:    binary.LittleEndian.Uint32(hdr[8:12]),
		FPS:       binary.LittleEndian.Uint32(hdr[12:16]),
		Width:     binary.LittleEndian.Uint16(hdr[16:18]),
		Height:    binary.LittleEndian.Uint16(hdr[18:20]),
		AudioRate: binary.LittleEndian.Uint32(hdr[20:24]),
		Packets:   binary.LittleEndian.Uint32(hdr[24:28]),
	}
	manifest := &Manifest{
		Frames:     r.header.Frames,
		FPS:        r.header.FPS,
		Width:      r.header.Width,
		Height:     r.header.Height,
		AudioRate:  r.header.AudioRate,
		Packets:    r.header.Packets,
		TotalBytes: info.Size(),
		SourcePath: path,
	}
	return r, manifest, nil
}

// Header returns the parsed container header.
func (r *PacketReader) Header() Header { return r.header }

// ReadPacket reads the next record. A checksum mismatch returns the packet
// together with a recoverable corrupt error; the caller decides to drop it
// so FEC can attempt reconstruction. io.EOF marks a clean end of stream.
func (r *PacketReader) ReadPacket() (*media.Packet, error) {
	if r.read >= r.header.Packets {
		return nil, io.EOF
	}
	buf := make([]byte, RecordHdrLen)
	if _, err := io.ReadFull(r.file, buf); err != nil {
		return nil, media.NewError(media.ClassValidation, "ingest", "truncated record header", 0, false)
	}
	p := &media.Packet{
		Kind:     media.PacketKind(buf[0]),
		ParityOf: media.PacketKind(buf[1]),
		Seq:      binary.LittleEndian.Uint32(buf[2:6]),
		BlockID:  binary.LittleEndian.Uint32(buf[6:10]),
		PTS:      binary.LittleEndian.Uint64(buf[10:18]),
	}
	payloadLen := binary.LittleEndian.Uint32(buf[18:22])
	if payloadLen > MaxPacketSize {
		return p, media.NewError(media.ClassValidation, "ingest", "record exceeds packet size limit", p.PTS, false)
	}
	digest := make([]byte, ShaLen)
	if _, err := io.ReadFull(r.file, digest); err != nil {
		return nil, media.NewError(media.ClassValidation, "ingest", "truncated digest", p.PTS, false)
	}
	p.Payload = make([]byte, payloadLen)
	if payloadLen > 0 {
		if _, err := io.ReadFull(r.file, p.Payload); err != nil {
			return nil, media.NewError(media.ClassValidation, "ingest", "truncated payload", p.PTS, false)
		}
	}
	p.SHA256 = digest
	r.read++
	sum := sha256.Sum256(p.Payload)
	if !equalBytes(sum[:], digest) {
		return p, media.NewError(media.ClassCorrupt, "ingest", "sha-256 mismatch on packet "+itoa32(p.Seq), p.PTS, true)
	}
	return p, nil
}

// Close releases the underlying file.
func (r *PacketReader) Close() error { return r.file.Close() }

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func itoa32(v uint32) string {
	if v == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
