package stream

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"avflow/internal/model"
)

// Fixed wire sizes used by the parser.
const (
	headerSize     = 17 // magic(6)+version(1)+trackCount(2)+packetCount(4)+flags(4)
	descFixedSize  = 32 // all descriptor fields except the trailing name
	packetHdrSize  = 37 // sync(2)+track(4)+seq(8)+pts(8)+dur(4)+flags(2)+tag(1)+len(4)+crc(4)
	crcFieldOffset = 33 // offset of crc inside the full 37-byte packet header
)

// Packet is one parsed AVPK packet with both raw tick fields and the unified
// nanosecond timeline positions derived from its track time base.
type Packet struct {
	Track      uint32
	Seq        uint64
	PTS        int64
	Duration   uint32
	Keyframe   bool
	ColorTag   uint8
	Payload    []byte
	Offset     int64
	ChecksumOK bool
	PTSNS      int64
	EndPTSNS   int64
}

// Reader parses an AVPK stream sequentially. It performs boundary, payload
// limit and CRC validation; semantic checks (PTS order, keyframes) live in
// the index layer so the parser stays reusable on its own.
type Reader struct {
	r           io.Reader
	tracks      []TrackDesc
	trackByID   map[uint32]int
	maxPayload  int
	packetCount uint32
	read        uint32
	offset      int64
}

// NewReader consumes the container header and returns a positioned Reader.
// A zero maxPayload selects MaxPayloadDefault.
func NewReader(r io.Reader, maxPayload int) (*Reader, error) {
	if maxPayload <= 0 {
		maxPayload = MaxPayloadDefault
	}
	rd := &Reader{r: r, maxPayload: maxPayload, trackByID: map[uint32]int{}}
	if err := rd.readHeader(); err != nil {
		return nil, err
	}
	return rd, nil
}

func (r *Reader) readFull(p []byte) error {
	n, err := io.ReadFull(r.r, p)
	r.offset += int64(n)
	return err
}

func (r *Reader) readHeader() error {
	hdr := make([]byte, headerSize)
	if err := r.readFull(hdr); err != nil {
		return model.NewFault(model.FaultTruncated, 0, 0, 0, 0, false,
			fmt.Sprintf("container header truncated: %v", err))
	}
	if string(hdr[:len(Magic)]) != Magic {
		return model.NewFault(model.FaultTruncated, 0, 0, 0, 0, false, "bad AVPK magic")
	}
	if hdr[len(Magic)] != Version {
		return model.NewFault(model.FaultTruncated, 0, 0, 0, 0, false,
			fmt.Sprintf("unsupported version %d", hdr[len(Magic)]))
	}
	trackCount := int(binary.LittleEndian.Uint16(hdr[7:]))
	r.packetCount = binary.LittleEndian.Uint32(hdr[9:])
	for i := 0; i < trackCount; i++ {
		t, err := r.readDesc()
		if err != nil {
			return err
		}
		if _, dup := r.trackByID[t.ID]; dup {
			return model.NewFault(model.FaultTruncated, t.ID, 0, 0, 0, false, "duplicate track id")
		}
		r.trackByID[t.ID] = len(r.tracks)
		r.tracks = append(r.tracks, t)
	}
	return nil
}

func (r *Reader) readDesc() (TrackDesc, error) {
	fixed := make([]byte, descFixedSize)
	if err := r.readFull(fixed); err != nil {
		return TrackDesc{}, model.NewFault(model.FaultTruncated, 0, 0, 0, 0, false, "track descriptor truncated")
	}
	t := TrackDesc{
		ID:               binary.LittleEndian.Uint32(fixed[0:]),
		Kind:             TrackKind(fixed[4]),
		TimeBase:         model.Rational{Num: int64(binary.LittleEndian.Uint32(fixed[5:])), Den: int64(binary.LittleEndian.Uint32(fixed[9:]))},
		ColorTag:         fixed[13],
		Width:            binary.LittleEndian.Uint16(fixed[14:]),
		Height:           binary.LittleEndian.Uint16(fixed[16:]),
		Channels:         fixed[18],
		ChannelLayout:    fixed[19],
		SampleRate:       binary.LittleEndian.Uint32(fixed[20:]),
		MaxKeyIntervalNS: int64(binary.LittleEndian.Uint64(fixed[24:])),
	}
	var nameLen [1]byte
	if err := r.readFull(nameLen[:]); err != nil {
		return t, model.NewFault(model.FaultTruncated, t.ID, 0, 0, 0, false, "track name truncated")
	}
	name := make([]byte, nameLen[0])
	if err := r.readFull(name); err != nil {
		return t, model.NewFault(model.FaultTruncated, t.ID, 0, 0, 0, false, "track name truncated")
	}
	t.Name = string(name)
	return t, nil
}

// Tracks returns the parsed track directory.
func (r *Reader) Tracks() []TrackDesc { return r.tracks }

// PacketCount returns the count declared in the container header.
func (r *Reader) PacketCount() uint32 { return r.packetCount }

// Next returns the next packet or io.EOF at a clean end of stream.
func (r *Reader) Next() (*Packet, error) {
	if r.packetCount > 0 && r.read >= r.packetCount {
		return nil, io.EOF
	}
	var sync [2]byte
	start := r.offset
	if err := r.readFull(sync[:]); err != nil {
		if errors.Is(err, io.EOF) && r.read == r.packetCount {
			return nil, io.EOF
		}
		return nil, model.NewFault(model.FaultTruncated, 0, uint64(r.read), 0, 0, true, "stream ends inside packet sync")
	}
	if sync[0] != Sync0 || sync[1] != Sync1 {
		return nil, model.NewFault(model.FaultTruncated, 0, uint64(r.read), 0, 0, false,
			fmt.Sprintf("lost packet boundary at offset %d", start))
	}
	hdr := make([]byte, packetHdrSize-2)
	if err := r.readFull(hdr); err != nil {
		return nil, model.NewFault(model.FaultTruncated, 0, uint64(r.read), 0, 0, true, "truncated packet header")
	}
	p := &Packet{
		Offset:   start,
		Track:    binary.LittleEndian.Uint32(hdr[0:]),
		Seq:      binary.LittleEndian.Uint64(hdr[4:]),
		PTS:      int64(binary.LittleEndian.Uint64(hdr[12:])),
		Duration: binary.LittleEndian.Uint32(hdr[20:]),
		Keyframe: binary.LittleEndian.Uint16(hdr[24:])&FlagKeyframe != 0,
		ColorTag: hdr[26],
	}
	payloadLen := int(binary.LittleEndian.Uint32(hdr[27:]))
	storedCRC := binary.LittleEndian.Uint32(hdr[crcFieldOffset-2:])
	if payloadLen > r.maxPayload {
		return nil, model.NewFault(model.FaultPayloadLimit, p.Track, p.Seq, 0, 0, false,
			fmt.Sprintf("payload %d bytes exceeds limit %d", payloadLen, r.maxPayload))
	}
	p.Payload = make([]byte, payloadLen)
	if err := r.readFull(p.Payload); err != nil {
		return nil, model.NewFault(model.FaultTruncated, p.Track, p.Seq, 0, 0, true, "truncated packet payload")
	}
	sum := crc32.ChecksumIEEE(hdr[:crcFieldOffset-2])
	sum = crc32.Update(sum, crc32.IEEETable, p.Payload)
	p.ChecksumOK = sum == storedCRC
	if idx, ok := r.trackByID[p.Track]; ok {
		tb := r.tracks[idx].TimeBase
		p.PTSNS = model.ToNanoseconds(p.PTS, tb)
		p.EndPTSNS = p.PTSNS + model.DurationNanoseconds(uint64(p.Duration), tb)
	} else {
		return nil, model.NewFault(model.FaultTruncated, p.Track, p.Seq, 0, 0, false, "packet references unknown track")
	}
	r.read++
	return p, nil
}
