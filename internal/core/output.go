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

// OutputTrack mirrors a track header in the archive stream.
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

// OutputPacket is one decoded output packet (payload excluded by default).
type OutputPacket struct {
	Slot        string
	TrackID     uint32
	PacketIndex int64
	DTSNS       int64
	PTSNS       int64
	DurationNS  int64
	Sync        bool
	Size        uint32
	Payload     []byte
}

func writeOutputHeader(bw *bufio.Writer, asset *media.Asset, plan *maptrack.Plan, packetCount int64) error {
	if _, err := bw.WriteString(outputMagic); err != nil {
		return err
	}
	if err := binary.Write(bw, binary.BigEndian, uint32(outputVersion)); err != nil {
		return err
	}
	if err := writeString16(bw, asset.ID); err != nil {
		return err
	}
	if err := binary.Write(bw, binary.BigEndian, uint32(len(plan.Assignments))); err != nil {
		return err
	}
	for _, a := range plan.Assignments {
		trk, ok := asset.TrackByIndex(a.TrackIndex)
		if !ok {
			return fmt.Errorf("core: plan references missing track index %d", a.TrackIndex)
		}
		if err := writeString16(bw, a.Slot); err != nil {
			return err
		}
		if err := binary.Write(bw, binary.BigEndian, a.TrackID); err != nil {
			return err
		}
		if err := bw.WriteByte(kindByte(trk.Kind)); err != nil {
			return err
		}
		if err := binary.Write(bw, binary.BigEndian, trk.Timescale); err != nil {
			return err
		}
		if err := binary.Write(bw, binary.BigEndian, trk.OriginNS); err != nil {
			return err
		}
		if err := binary.Write(bw, binary.BigEndian, uint64(trk.PacketCount)); err != nil {
			return err
		}
	}
	if err := binary.Write(bw, binary.BigEndian, uint64(packetCount)); err != nil {
		return err
	}
	return nil
}

func writePacket(bw *bufio.Writer, slot string, pkt media.Packet, payload []byte) error {
	if err := writeString16(bw, slot); err != nil {
		return err
	}
	if err := binary.Write(bw, binary.BigEndian, pkt.SourceTrackID); err != nil {
		return err
	}
	if err := binary.Write(bw, binary.BigEndian, pkt.PacketIndex); err != nil {
		return err
	}
	if err := binary.Write(bw, binary.BigEndian, pkt.DTSNS); err != nil {
		return err
	}
	if err := binary.Write(bw, binary.BigEndian, pkt.PTSNS); err != nil {
		return err
	}
	if err := binary.Write(bw, binary.BigEndian, pkt.DurationNS); err != nil {
		return err
	}
	sync := byte(0)
	if pkt.Sync {
		sync = 1
	}
	if err := bw.WriteByte(sync); err != nil {
		return err
	}
	if err := binary.Write(bw, binary.BigEndian, uint32(len(payload))); err != nil {
		return err
	}
	if _, err := bw.Write(payload); err != nil {
		return err
	}
	return nil
}

func writeString16(w io.Writer, s string) error {
	if len(s) > 0xffff {
		return fmt.Errorf("core: string too long: %d", len(s))
	}
	if err := binary.Write(w, binary.BigEndian, uint16(len(s))); err != nil {
		return err
	}
	_, err := io.WriteString(w, s)
	return err
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

// ReadOutput decodes an archive stream. When wantPayload is set each packet's
// bytes are retained; otherwise only metadata is read (the reader still walks
// past the payload to reach the next record).
func ReadOutput(path string) (*OutputHeader, []OutputPacket, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fault.New(fault.CodeVerify, fault.Location{SourcePath: path},
			"open output failed").Wrap(err).WithOp("core.ReadOutput")
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<16)
	magic := make([]byte, len(outputMagic))
	if _, err := io.ReadFull(br, magic); err != nil {
		return nil, nil, corruptOutput(path, "short magic")
	}
	if string(magic) != outputMagic {
		return nil, nil, corruptOutput(path, "bad magic %q", string(magic))
	}
	var version uint32
	if err := binary.Read(br, binary.BigEndian, &version); err != nil {
		return nil, nil, corruptOutput(path, "short version")
	}
	if version != outputVersion {
		return nil, nil, corruptOutput(path, "unsupported output version %d", version)
	}
	assetID, err := readString16(br)
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
		if t.Slot, err = readString16(br); err != nil {
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
		if p.Slot, err = readString16(br); err != nil {
			return nil, nil, corruptOutput(path, "short packet slot at %d", i)
		}
		if err := binary.Read(br, binary.BigEndian, &p.TrackID); err != nil {
			return nil, nil, corruptOutput(path, "short packet track at %d", i)
		}
		if err := binary.Read(br, binary.BigEndian, &p.PacketIndex); err != nil {
			return nil, nil, corruptOutput(path, "short packet index at %d", i)
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
		if err := binary.Read(br, binary.BigEndian, &p.Size); err != nil {
			return nil, nil, corruptOutput(path, "short size at %d", i)
		}
		body := make([]byte, p.Size)
		if _, err := io.ReadFull(br, body); err != nil {
			return nil, nil, corruptOutput(path, "short payload at %d", i)
		}
		p.Payload = body
		packets = append(packets, p)
	}
	return hdr, packets, nil
}

func readString16(r io.Reader) (string, error) {
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
