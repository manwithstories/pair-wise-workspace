package core

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"mediabridge/internal/fault"
	"mediabridge/internal/maptrack"
	"mediabridge/internal/media"
)

// The archive stream is a self-describing record of encoded packets. It is not
// a playable container: its purpose is a byte-exact, independently verifiable
// copy of encoded samples with full source attribution.
//
// Layout (all integers big-endian), "MBRX1":
//
//	magic        [5]byte
//	version      uint32
//	assetID      str16
//	trackCount   uint32
//	  slot       str16
//	  trackID    u32
//	  kind       u8 ('v','a','x')
//	  timescale  u32
//	  originNS   i64
//	  packets    u64
//	packetCount  u64
//	per packet:
//	  slot         str16
//	  sourceTrack  u32
//	  sourceIndex  i64
//	  sourceOffset i64
//	  sourceSize   i64
//	  dts          i64
//	  pts          i64
//	  dur          i64
//	  sync         u8
//	  size         u32
//	  payload      [size]
type OutputTrack struct {
	Slot        string
	TrackID     uint32
	Kind        media.TrackKind
	Timescale   uint32
	OriginNS    int64
	PacketCount uint64
}

// OutputHeader is the decoded stream header.
type OutputHeader struct {
	AssetID string
	Tracks  []OutputTrack
	Packets uint64
}

// OutputPacket is one decoded output packet.
type OutputPacket struct {
	Slot         string
	SourceTrack  uint32
	SourceIndex  int64
	SourceOffset int64
	SourceSize   int64
	DTSNS        int64
	PTSNS        int64
	DurationNS   int64
	Sync         bool
	Payload      []byte
}

// Writer writes an archive stream to a file, optionally over an existing
// committed prefix (for resume). It maintains a running SHA-256 of all bytes
// written, including any copied prefix.
type Writer struct {
	f      *os.File
	bw     *bufio.Writer
	hasher hashLike
}

type hashLike interface {
	Write([]byte) (int, error)
	Sum([]byte) []byte
}

// NewWriter opens path for writing. If resumeFrom > 0 the file is truncated to
// that length and the existing bytes are copied through the hasher, so the
// final digest covers the whole file. When resumeFrom == 0 the file is reset.
func NewWriter(path string, resumeFrom int64, copyPrefix func(io.Writer) (int64, error)) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	hasher := sha256New()
	w := &Writer{f: f, hasher: hasher}
	if resumeFrom > 0 && copyPrefix != nil {
		n, err := copyPrefix(hasher)
		if err != nil {
			f.Close()
			return nil, err
		}
		if n != resumeFrom {
			f.Close()
			return nil, fmt.Errorf("core: prefix copy %d != resume point %d", n, resumeFrom)
		}
		if err := f.Truncate(resumeFrom); err != nil {
			f.Close()
			return nil, err
		}
		if _, err := f.Seek(resumeFrom, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
	} else {
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
	}
	w.bw = bufio.NewWriterSize(io.MultiWriter(f, hasher), 256<<10)
	return w, nil
}

// WriteHeader writes the stream header.
func (w *Writer) WriteHeader(asset *media.Asset, plan *maptrack.Plan, packetCount int64) error {
	if _, err := w.bw.WriteString(OutputFormat); err != nil {
		return err
	}
	if err := binary.Write(w.bw, binary.BigEndian, uint32(outputVersion)); err != nil {
		return err
	}
	if err := writeStr16(w.bw, asset.ID); err != nil {
		return err
	}
	assignments := OrderedAssignments(plan.Assignments)
	if err := binary.Write(w.bw, binary.BigEndian, uint32(len(assignments))); err != nil {
		return err
	}
	for _, a := range assignments {
		trk, ok := asset.TrackByIndex(a.TrackIndex)
		if !ok {
			return fmt.Errorf("core: plan references missing track %d", a.TrackIndex)
		}
		if err := writeStr16(w.bw, a.Slot); err != nil {
			return err
		}
		if err := binary.Write(w.bw, binary.BigEndian, a.TrackID); err != nil {
			return err
		}
		if err := w.bw.WriteByte(kindByte(trk.Kind)); err != nil {
			return err
		}
		if err := binary.Write(w.bw, binary.BigEndian, trk.Timescale); err != nil {
			return err
		}
		if err := binary.Write(w.bw, binary.BigEndian, trk.OriginNS); err != nil {
			return err
		}
		if err := binary.Write(w.bw, binary.BigEndian, uint64(trk.PacketCount)); err != nil {
			return err
		}
	}
	return binary.Write(w.bw, binary.BigEndian, uint64(packetCount))
}

// WritePacket writes one packet record including its payload.
func (w *Writer) WritePacket(slot string, pkt media.Packet, payload []byte) error {
	if err := writeStr16(w.bw, slot); err != nil {
		return err
	}
	if err := binary.Write(w.bw, binary.BigEndian, pkt.SourceTrackID); err != nil {
		return err
	}
	fields := []int64{
		pkt.PacketIndex, pkt.SourceOffset, pkt.SourceSize,
		pkt.DTSNS, pkt.PTSNS, pkt.DurationNS,
	}
	for _, v := range fields {
		if err := binary.Write(w.bw, binary.BigEndian, v); err != nil {
			return err
		}
	}
	sync := byte(0)
	if pkt.Sync {
		sync = 1
	}
	if err := w.bw.WriteByte(sync); err != nil {
		return err
	}
	if err := binary.Write(w.bw, binary.BigEndian, uint32(len(payload))); err != nil {
		return err
	}
	_, err := w.bw.Write(payload)
	return err
}

// Flush drains buffered bytes to the file.
func (w *Writer) Flush() error { return w.bw.Flush() }

// Sync flushes and fsyncs.
func (w *Writer) Sync() error {
	if err := w.bw.Flush(); err != nil {
		return err
	}
	return w.f.Sync()
}

// Size returns the current on-disk size (after flush).
func (w *Writer) Size() (int64, error) {
	if err := w.bw.Flush(); err != nil {
		return 0, err
	}
	fi, err := w.f.Stat()
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// Digest returns the running hex SHA-256 of all bytes written (prefix included).
func (w *Writer) Digest() string { return hexEncode(w.hasher.Sum(nil)) }

// Close closes the underlying file.
func (w *Writer) Close() error {
	if err := w.bw.Flush(); err != nil {
		w.f.Close()
		return err
	}
	return w.f.Close()
}

// ReadOutput decodes an archive stream.
func ReadOutput(path string) (*OutputHeader, []OutputPacket, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fault.New(fault.CodeVerify, fault.Location{SourcePath: path},
			"open output failed").Wrap(err).WithOp("core.ReadOutput")
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<16)
	magic := make([]byte, len(OutputFormat))
	if _, err := io.ReadFull(br, magic); err != nil {
		return nil, nil, corruptOutput(path, "short magic")
	}
	if string(magic) != OutputFormat {
		return nil, nil, corruptOutput(path, "bad magic %q", string(magic))
	}
	var version uint32
	if err := binary.Read(br, binary.BigEndian, &version); err != nil {
		return nil, nil, corruptOutput(path, "short version")
	}
	if version != outputVersion {
		return nil, nil, corruptOutput(path, "unsupported output version %d", version)
	}
	assetID, err := readStr16(br)
	if err != nil {
		return nil, nil, corruptOutput(path, "short asset id")
	}
	var trackCount uint32
	if err := binary.Read(br, binary.BigEndian, &trackCount); err != nil {
		return nil, nil, corruptOutput(path, "short track count")
	}
	hdr := &OutputHeader{AssetID: assetID}
	for i := uint32(0); i < trackCount; i++ {
		var t OutputTrack
		if t.Slot, err = readStr16(br); err != nil {
			return nil, nil, corruptOutput(path, "short track slot")
		}
		if err := binary.Read(br, binary.BigEndian, &t.TrackID); err != nil {
			return nil, nil, corruptOutput(path, "short track id")
		}
		kb, err := br.ReadByte()
		if err != nil {
			return nil, nil, corruptOutput(path, "short track kind")
		}
		t.Kind = byteKind(kb)
		if err := binary.Read(br, binary.BigEndian, &t.Timescale); err != nil {
			return nil, nil, corruptOutput(path, "short timescale")
		}
		if err := binary.Read(br, binary.BigEndian, &t.OriginNS); err != nil {
			return nil, nil, corruptOutput(path, "short origin")
		}
		if err := binary.Read(br, binary.BigEndian, &t.PacketCount); err != nil {
			return nil, nil, corruptOutput(path, "short packet count")
		}
		hdr.Tracks = append(hdr.Tracks, t)
	}
	if err := binary.Read(br, binary.BigEndian, &hdr.Packets); err != nil {
		return nil, nil, corruptOutput(path, "short total packet count")
	}
	packets := make([]OutputPacket, 0, hdr.Packets)
	for i := uint64(0); i < hdr.Packets; i++ {
		var p OutputPacket
		if p.Slot, err = readStr16(br); err != nil {
			return nil, nil, corruptOutput(path, "short packet slot at %d", i)
		}
		if err := binary.Read(br, binary.BigEndian, &p.SourceTrack); err != nil {
			return nil, nil, corruptOutput(path, "short source track at %d", i)
		}
		if err := binary.Read(br, binary.BigEndian, &p.SourceIndex); err != nil {
			return nil, nil, corruptOutput(path, "short source index at %d", i)
		}
		if err := binary.Read(br, binary.BigEndian, &p.SourceOffset); err != nil {
			return nil, nil, corruptOutput(path, "short source offset at %d", i)
		}
		if err := binary.Read(br, binary.BigEndian, &p.SourceSize); err != nil {
			return nil, nil, corruptOutput(path, "short source size at %d", i)
		}
		if err := binary.Read(br, binary.BigEndian, &p.DTSNS); err != nil {
			return nil, nil, corruptOutput(path, "short dts at %d", i)
		}
		if err := binary.Read(br, binary.BigEndian, &p.PTSNS); err != nil {
			return nil, nil, corruptOutput(path, "short pts at %d", i)
		}
		if err := binary.Read(br, binary.BigEndian, &p.DurationNS); err != nil {
			return nil, nil, corruptOutput(path, "short dur at %d", i)
		}
		sb, err := br.ReadByte()
		if err != nil {
			return nil, nil, corruptOutput(path, "short sync at %d", i)
		}
		p.Sync = sb == 1
		var size uint32
		if err := binary.Read(br, binary.BigEndian, &size); err != nil {
			return nil, nil, corruptOutput(path, "short size at %d", i)
		}
		body := make([]byte, size)
		if _, err := io.ReadFull(br, body); err != nil {
			return nil, nil, corruptOutput(path, "short payload at %d", i)
		}
		p.Payload = body
		packets = append(packets, p)
	}
	return hdr, packets, nil
}

func writeStr16(w io.Writer, s string) error {
	if len(s) > 0xffff {
		return fmt.Errorf("core: string too long: %d", len(s))
	}
	if err := binary.Write(w, binary.BigEndian, uint16(len(s))); err != nil {
		return err
	}
	_, err := io.WriteString(w, s)
	return err
}

func readStr16(r io.Reader) (string, error) {
	var n uint16
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return "", err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func kindByte(k media.TrackKind) byte {
	switch k {
	case media.KindVideo:
		return 'v'
	case media.KindAudio:
		return 'a'
	default:
		return 'x'
	}
}

func byteKind(b byte) media.TrackKind {
	switch b {
	case 'v':
		return media.KindVideo
	case 'a':
		return media.KindAudio
	default:
		return media.KindAux
	}
}

func corruptOutput(path, format string, args ...any) error {
	return fault.New(fault.CodeVerify, fault.Location{SourcePath: path},
		"output stream corrupt: "+format, args...).WithOp("core.ReadOutput")
}

// FileSampleSource reads samples from an open source file by offset.
type FileSampleSource struct {
	f *os.File
}

// NewFileSampleSource wraps an open file.
func NewFileSampleSource(f *os.File) *FileSampleSource { return &FileSampleSource{f: f} }

// ReadSample implements SampleSource.
func (s *FileSampleSource) ReadSample(trackID uint32, offset, size int64) ([]byte, error) {
	if size < 0 || size > media.MaxPacketBytes {
		return nil, fmt.Errorf("sample size %d out of range", size)
	}
	buf := make([]byte, size)
	if _, err := s.f.ReadAt(buf, offset); err != nil {
		return nil, err
	}
	return buf, nil
}
