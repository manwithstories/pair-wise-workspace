package demux

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"

	"mediapipe/internal/ebml"
	"mediapipe/internal/media"
)

// readCluster parses one Cluster element, appending its packets. Unknown-size
// clusters end when readChildren sees a Segment-level sibling.
func (p *parser) readCluster(el ebml.Element) error {
	ci := ClusterInfo{Offset: el.Offset, PayloadFrom: el.DataOffset(), CRCOffset: -1}
	prev := p.curCluster
	p.curCluster = &ci
	defer func() {
		if el.Unknown {
			ci.PayloadTo = p.pos()
		} else {
			ci.PayloadTo = el.End()
		}
		p.res.Clusters = append(p.res.Clusters, ci)
		p.curCluster = prev
	}()

	p.clusterTS = 0
	return p.readChildren(el, func(child ebml.Element) error {
		switch child.ID {
		case ebml.IDTimestamp:
			v, err := readUint(p.r, child)
			if err != nil {
				return err
			}
			p.clusterTS = int64(v) * p.tick
			return nil
		case ebml.IDSimpleBlock:
			return p.readSimpleBlock(child)
		case ebml.IDBlockGroup:
			return p.readBlockGroup(child)
		case ebml.IDCRC32:
			return p.readClusterCRC(child, &ci)
		default:
			return p.r.Skip(child.Payload)
		}
	})
}

// readClusterCRC records a Cluster CRC-32 element. Matroska defines the CRC as
// covering all bytes of the parent element after the CRC element itself.
func (p *parser) readClusterCRC(el ebml.Element, ci *ClusterInfo) error {
	if el.Payload != 4 {
		return media.At("demux.crc", el.Offset, media.ClassFatal,
			"cluster CRC-32 has %d bytes", el.Payload)
	}
	b, err := p.r.ReadBytes(el.Payload)
	if err != nil {
		return err
	}
	ci.HasCRC = true
	ci.CRCValue = binary.BigEndian.Uint32(b)
	ci.CRCOffset = el.Offset
	ci.CRCFrom = el.End()
	return nil
}

// VerifyClusterCRC recomputes the CRC-32 over the bytes following the CRC
// element and compares it with the stored value. raw is the complete input
// file, or any buffer whose indexing uses the same absolute offsets.
func (ci ClusterInfo) VerifyClusterCRC(raw []byte) bool {
	if !ci.HasCRC {
		return true
	}
	from, to := ci.CRCFrom, ci.PayloadTo
	if from < 0 || to > int64(len(raw)) || from > to {
		return false
	}
	return crc32.ChecksumIEEE(raw[from:to]) == ci.CRCValue
}

func (p *parser) readSimpleBlock(el ebml.Element) error {
	data, err := p.readBlockPayload(el)
	if err != nil {
		return err
	}
	// SimpleBlock flags bit 0x80 marks a keyframe.
	if len(data) < 4 {
		return media.At("demux.simpleblock", el.Offset, media.ClassFatal,
			"SimpleBlock payload too short: %d bytes", len(data))
	}
	keyframe := data[3]&0x80 != 0
	return p.emitBlock(el.Offset, data, keyframe, false)
}

func (p *parser) readBlockGroup(el ebml.Element) error {
	var blockData []byte
	var blockOffset int64
	keyframe := true
	var haveBlock bool
	err := p.readChildren(el, func(child ebml.Element) error {
		switch child.ID {
		case ebml.IDBlock:
			if haveBlock {
				return media.At("demux.blockgroup", child.Offset, media.ClassFatal,
					"BlockGroup contains more than one Block")
			}
			b, err := p.readBlockPayload(child)
			if err != nil {
				return err
			}
			blockData, blockOffset, haveBlock = b, child.Offset, true
			return nil
		case ebml.IDReferenceBlock:
			keyframe = false
			return p.r.Skip(child.Payload)
		case ebml.IDBlockDuration:
			return p.r.Skip(child.Payload)
		default:
			return p.r.Skip(child.Payload)
		}
	})
	if err != nil {
		return err
	}
	if !haveBlock {
		return media.At("demux.blockgroup", el.Offset, media.ClassFatal, "BlockGroup without Block")
	}
	return p.emitBlock(blockOffset, blockData, keyframe, true)
}

// readBlockPayload reads a Block/SimpleBlock payload while flagging truncation
// that happens inside announced media data.
func (p *parser) readBlockPayload(el ebml.Element) ([]byte, error) {
	if el.Unknown {
		return nil, media.At("demux.block", el.Offset, media.ClassFatal, "block has unknown size")
	}
	b, err := p.r.ReadBytes(el.Payload)
	if err != nil {
		p.res.Truncated = true
		return nil, err
	}
	return b, nil
}

// emitBlock parses the frame header and lacing, then appends one Packet per
// lace.
func (p *parser) emitBlock(offset int64, data []byte, keyframe, grouped bool) error {
	track, n, err := readVintFrom(data)
	if err != nil {
		return media.At("demux.block", offset, media.ClassFatal, "block track number: %v", err)
	}
	if n+3 > len(data) {
		return media.At("demux.block", offset, media.ClassFatal,
			"block payload too short after track number: %d bytes", len(data))
	}
	if _, ok := p.tracks[track]; !ok {
		return media.AtTrack("demux.block", offset, track, media.ClassFatal,
			"block references undeclared track %d", track)
	}
	if p.curCluster != nil {
		p.curCluster.Packets++
	}
	rel := int16(binary.BigEndian.Uint16(data[n:]))
	flags := data[n+2]
	body := data[n+3:]

	ts := p.clusterTS + int64(rel)*p.tick
	if ts < 0 {
		return media.AtTrack("demux.block", offset, track, media.ClassFatal,
			"negative absolute timestamp %d", ts)
	}
	dts := media.Timestamp(ts)

	var frames [][]byte
	if flags&0x06 == 0 {
		frames = [][]byte{body}
	} else {
		frames, err = lace(body, (flags>>1)&0x03)
		if err != nil {
			return media.AtTrack("demux.block", offset, track, media.ClassFatal,
				"lacing: %v", err)
		}
	}

	dur := p.tracks[track].track.DefaultDuration
	for _, frame := range frames {
		if p.limits.MaxPackets > 0 && len(p.res.Packets) >= p.limits.MaxPackets {
			return media.At("demux.packets", offset, media.ClassFatal,
				"packet count exceeds limit %d", p.limits.MaxPackets)
		}
		crc := crc32.ChecksumIEEE(frame)
		pkt := media.Packet{
			Track:    track,
			DTS:      dts,
			PTS:      dts,
			Duration: dur,
			Keyframe: keyframe,
			Data:     frame,
			CRC:      crc,
			Sequence: p.seq,
		}
		p.seq++
		if grouped && dur == 0 {
			pkt.Duration = 0
		}
		p.res.Packets = append(p.res.Packets, pkt)
	}
	return nil
}

// lace splits a block body into frames according to the Matroska lacing mode.
func lace(body []byte, mode byte) ([][]byte, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("empty laced body")
	}
	count := int(body[0]) + 1
	rest := body[1:]
	sizes := make([]int, count)
	switch mode {
	case 1: // Xiph
		pos := 0
		for i := 0; i < count-1; i++ {
			size := 0
			for {
				if pos >= len(rest) {
					return nil, fmt.Errorf("xiph lace size %d truncated", i)
				}
				b := rest[pos]
				pos++
				size += int(b)
				if b != 255 {
					break
				}
			}
			sizes[i] = size
		}
		rest = rest[pos:]
	case 2: // fixed
		if len(rest)%count != 0 {
			return nil, fmt.Errorf("fixed lacing: %d bytes not divisible by %d frames", len(rest), count)
		}
		size := len(rest) / count
		for i := range sizes {
			sizes[i] = size
		}
	case 3: // EBML
		pos := 0
		for i := 0; i < count-1; i++ {
			size, n, err := readVintFrom(rest[pos:])
			if err != nil {
				return nil, fmt.Errorf("ebml lace size %d: %w", i, err)
			}
			pos += n
			sizes[i] = int(size)
		}
		rest = rest[pos:]
	default:
		return nil, fmt.Errorf("invalid lacing mode %d", mode)
	}
	frames := make([][]byte, 0, count)
	total := 0
	for i := 0; i < count-1; i++ {
		if sizes[i] < 0 || total+sizes[i] > len(rest) {
			return nil, fmt.Errorf("lace %d size %d overruns %d remaining bytes", i, sizes[i], len(rest))
		}
		frames = append(frames, rest[total:total+sizes[i]])
		total += sizes[i]
	}
	frames = append(frames, rest[total:])
	return frames, nil
}

// readVintFrom decodes one EBML unsigned VINT from b, returning the payload
// value and encoded length. It rejects leading zero bytes and overlong VINTs.
func readVintFrom(b []byte) (uint64, int, error) {
	if len(b) == 0 {
		return 0, 0, io.ErrUnexpectedEOF
	}
	first := b[0]
	if first == 0 {
		return 0, 0, fmt.Errorf("invalid VINT leading byte 0x00")
	}
	length := 1
	mask := byte(0x80)
	for first&mask == 0 {
		length++
		mask >>= 1
	}
	if length > 8 {
		return 0, 0, fmt.Errorf("VINT longer than 8 bytes")
	}
	if len(b) < length {
		return 0, 0, io.ErrUnexpectedEOF
	}
	value := uint64(first &^ mask)
	for i := 1; i < length; i++ {
		value = value<<8 | uint64(b[i])
	}
	return value, length, nil
}
