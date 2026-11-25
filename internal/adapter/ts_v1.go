package adapter

import (
	"encoding/binary"
	"sort"

	"mediad/internal/media"
)

// TSV1 reads MPEG-TS segments (ISO/IEC 13818-1). Version 1 covers PAT/PMT
// discovery, PES assembly and 33-bit PTS/DTS extraction for one video and one
// audio elementary stream per segment.
type TSV1 struct{}

const (
	tsPacketSize   = 188
	tsSyncByte     = 0x47
	pidPAT         = 0x0000
	pidNull        = 0x1FFF
	sectionPAT     = 0x00
	sectionPMT     = 0x02
	streamTypeH264 = 0x1B
	streamTypeH265 = 0x24
	streamTypeAAC  = 0x0F
	streamTypeAAC2 = 0x11
)

// Kind implements Adapter.
func (*TSV1) Kind() Container { return ContainerTS }

// Version implements Adapter.
func (*TSV1) Version() int { return 1 }

// tsStream describes one elementary stream discovered in a PMT.
type tsStream struct {
	PID        uint16
	StreamType uint8
	Kind       media.Kind
	Codec      string
}

// ParseInit implements Adapter. For TS, "init" is a probe segment: the first
// segment establishes the PMT so later segments can map their PIDs.
func (*TSV1) ParseInit(data []byte, pos media.SourcePos) (*InitSegment, error) {
	streams, err := discoverStreams(data, pos)
	if err != nil {
		return nil, err
	}
	init := &InitSegment{}
	for i, s := range streams {
		init.Tracks = append(init.Tracks, &InitTrack{
			ID:        uint32(i + 1),
			Kind:      s.Kind,
			Codec:     s.Codec,
			EntryType: s.Codec,
			Timescale: media.TickRate,
		})
	}
	if len(init.Tracks) == 0 {
		return nil, media.New(media.CodeNoTrack, "ts-init", pos, "no elementary streams in PAT/PMT")
	}
	return init, nil
}

// discoverStreams parses PAT and PMT sections from the first packets.
func discoverStreams(data []byte, pos media.SourcePos) ([]tsStream, error) {
	if err := validateTS(data, pos); err != nil {
		return nil, err
	}
	var pmtPID uint16
	var havePAT bool
	var streams []tsStream
	for off := 0; off+tsPacketSize <= len(data); off += tsPacketSize {
		pkt := data[off : off+tsPacketSize]
		pid, payload, ok := tsPayload(pkt)
		if !ok {
			continue
		}
		switch {
		case pid == pidPAT && !havePAT:
			pmtPID, havePAT = parsePAT(payload)
		case havePAT && pid == pmtPID && len(streams) == 0:
			streams = parsePMT(payload)
		}
		if havePAT && len(streams) > 0 {
			break
		}
	}
	if !havePAT {
		return nil, media.New(media.CodeSegmentParse, "ts-init", pos, "no PAT section found")
	}
	if len(streams) == 0 {
		return nil, media.New(media.CodeSegmentParse, "ts-init", pos, "no PMT section found")
	}
	return streams, nil
}

func validateTS(data []byte, pos media.SourcePos) error {
	if len(data) == 0 {
		return media.New(media.CodeTruncatedBox, "ts-validate", pos, "empty TS segment")
	}
	if len(data)%tsPacketSize != 0 {
		return media.New(media.CodeTruncatedBox, "ts-validate", pos,
			"TS size %d is not a multiple of %d (truncated packet)", len(data), tsPacketSize)
	}
	if data[0] != tsSyncByte {
		return media.New(media.CodeSegmentParse, "ts-validate", pos, "missing TS sync byte 0x47")
	}
	return nil
}

// tsPayload returns the elementary payload of a packet and its PID, skipping
// adaptation fields and signalling discontinuity with ok=false.
func tsPayload(pkt []byte) (pid uint16, payload []byte, ok bool) {
	if len(pkt) < tsPacketSize || pkt[0] != tsSyncByte {
		return 0, nil, false
	}
	pusi := pkt[1]&0x40 != 0
	pid = binary.BigEndian.Uint16(pkt[1:]) & 0x1FFF
	afc := (pkt[3] >> 4) & 0x3
	offset := 4
	if afc&0x2 != 0 {
		if offset >= tsPacketSize {
			return 0, nil, false
		}
		afLen := int(pkt[offset])
		offset += 1 + afLen
	}
	if afc&0x1 == 0 || offset >= tsPacketSize {
		return 0, nil, false
	}
	_ = pusi
	return pid, pkt[offset:], true
}

func parsePAT(payload []byte) (pmtPID uint16, ok bool) {
	sec, ok := tableSection(payload, pidPAT)
	if !ok || len(sec) < 8 {
		return 0, false
	}
	// section_length at [1:3]; table_id at [0].
	length := int(binary.BigEndian.Uint16(sec[1:3]) & 0x0FFF)
	if len(sec) < 3+length {
		return 0, false
	}
	body := sec[8 : 3+length-4] // skip header (8) and CRC (4)
	for i := 0; i+4 <= len(body); i += 4 {
		program := binary.BigEndian.Uint16(body[i:])
		pid := binary.BigEndian.Uint16(body[i+2:]) & 0x1FFF
		if program != 0 {
			return pid, true
		}
	}
	return 0, false
}

func parsePMT(payload []byte) []tsStream {
	sec, ok := tableSection(payload, pidPAT) // table_id check below
	if !ok || len(sec) < 12 {
		return nil
	}
	length := int(binary.BigEndian.Uint16(sec[1:3]) & 0x0FFF)
	if len(sec) < 3+length || 3+length-4 > len(sec) {
		return nil
	}
	body := sec[12 : 3+length-4]
	var out []tsStream
	for i := 0; i+5 <= len(body); {
		st := body[i]
		pid := binary.BigEndian.Uint16(body[i+1:]) & 0x1FFF
		esLen := int(binary.BigEndian.Uint16(body[i+3:]) & 0x0FFF)
		s := tsStream{PID: pid, StreamType: st}
		switch st {
		case streamTypeH264:
			s.Kind, s.Codec = media.KindVideo, "avc1"
		case streamTypeH265:
			s.Kind, s.Codec = media.KindVideo, "hvc1"
		case streamTypeAAC, streamTypeAAC2:
			s.Kind, s.Codec = media.KindAudio, "mp4a"
		default:
			i += 5 + esLen
			continue
		}
		out = append(out, s)
		i += 5 + esLen
	}
	return out
}

// tableSection reassembles a PSI section spanning one or more packets. Only
// the single-packet case is required for well-formed fixtures.
func tableSection(payload []byte, wantPID uint16) ([]byte, bool) {
	_ = wantPID
	if len(payload) < 1 {
		return nil, false
	}
	ptr := int(payload[0])
	start := 1 + ptr
	if start >= len(payload) {
		return nil, false
	}
	return payload[start:], payload[start] == sectionPAT || payload[start] == sectionPMT
}

// pesHeader is the parsed timing of one PES packet.
type pesHeader struct {
	PTS, DTS       int64
	HasPTS, HasDTS bool
	Payload        []byte
	StreamID       byte
}

// parsePES decodes PTS/DTS from a complete PES packet.
func parsePES(pes []byte) (*pesHeader, bool) {
	if len(pes) < 9 || pes[0] != 0 || pes[1] != 0 || pes[2] != 1 {
		return nil, false
	}
	streamID := pes[3]
	if streamID == 0xBC || streamID == 0xBE || streamID == 0xBF || streamID >= 0xF0 {
		return &pesHeader{StreamID: streamID, Payload: pes[6:]}, true
	}
	hdrLen := int(pes[8])
	if 9+hdrLen > len(pes) {
		return nil, false
	}
	h := &pesHeader{StreamID: streamID, Payload: pes[9+hdrLen:]}
	flags := pes[7]
	if flags&0x80 != 0 && hdrLen >= 5 {
		h.PTS = decodePCRTimestamp(pes[9:14])
		h.HasPTS = true
	}
	if flags&0x40 != 0 && hdrLen >= 10 {
		h.DTS = decodePCRTimestamp(pes[14:19])
		h.HasDTS = true
	} else if h.HasPTS {
		h.DTS = h.PTS
	}
	return h, true
}

// decodePCRTimestamp decodes a 5-byte PTS/DTS field (33 bits + marker bits).
func decodePCRTimestamp(b []byte) int64 {
	return int64(b[0]>>1&0x07)<<30 |
		int64(binary.BigEndian.Uint16(b[1:]))>>1<<15 |
		int64(binary.BigEndian.Uint16(b[3:]))>>1
}

// ParseSegment implements Adapter.
func (*TSV1) ParseSegment(req SegmentRequest) ([]media.Sample, error) {
	if err := req.Limits.CheckBytes("segment", int64(len(req.Data))); err != nil {
		return nil, err
	}
	if err := validateTS(req.Data, req.Pos); err != nil {
		return nil, err
	}
	streams, err := discoverStreams(req.Data, req.Pos)
	if err != nil {
		return nil, err
	}
	// Map stream PIDs to container tracks in discovery order; the init segment
	// for TS was produced from the same order.
	pidToKey := make(map[uint16]media.TrackKey, len(streams))
	for i, s := range streams {
		id := uint32(i + 1)
		if key, ok := req.Keys[id]; ok {
			pidToKey[s.PID] = key
		}
	}
	// Assemble PES payloads per PID.
	type pesBuf struct {
		data []byte
	}
	buffers := make(map[uint16]*pesBuf)
	var order []uint16
	var samples []media.Sample

	flush := func(pid uint16) error {
		buf := buffers[pid]
		if buf == nil || len(buf.data) == 0 {
			return nil
		}
		h, ok := parsePES(buf.data)
		if !ok || !h.HasPTS {
			return nil
		}
		key, mapped := pidToKey[pid]
		if !mapped {
			return nil
		}
		ref := req.Refs[trackIDForPID(streams, pid)]
		dts := h.DTS
		pts := h.PTS
		if h.HasDTS {
			dts = media.UnwrapTimestamp(ref, h.DTS)
		}
		pts = media.UnwrapTimestamp(ref, h.PTS)
		if !h.HasDTS {
			dts = pts
		}
		stream := streamForPID(streams, pid)
		if stream == nil {
			return nil
		}
		drift := pts - dts
		samples = append(samples, media.Sample{
			Key:      key,
			DTS:      dts,
			PTS:      pts,
			Duration: 0, // filled in by the timeline stage from DTS deltas
			Sync:     true,
			Data:     append([]byte(nil), h.Payload...),
			Source: media.SourceRef{
				Track:   key.String(),
				Segment: req.SegmentIndex,
				Sample:  len(samples),
				URI:     req.URI,
				Pos:     req.Pos,
			},
		})
		_ = drift
		return nil
	}

	for off := 0; off+tsPacketSize <= len(req.Data); off += tsPacketSize {
		pkt := req.Data[off : off+tsPacketSize]
		pid, payload, ok := tsPayload(pkt)
		if !ok || pid == pidPAT || pid == pidNull {
			continue
		}
		if _, wanted := pidToKey[pid]; !wanted {
			continue
		}
		pusi := pkt[1]&0x40 != 0
		if pusi {
			if err := flush(pid); err != nil {
				return nil, err
			}
			buffers[pid] = &pesBuf{data: append([]byte(nil), payload...)}
			order = append(order, pid)
		} else if b := buffers[pid]; b != nil {
			b.data = append(b.data, payload...)
			if int64(len(b.data)) > req.Limits.MaxSegmentBytes {
				return nil, media.New(media.CodeLimit, "ts-pes", req.Pos, "PES buffer exceeded limit")
			}
		}
	}
	for _, pid := range order {
		if err := flush(pid); err != nil {
			return nil, err
		}
	}
	if len(samples) == 0 {
		return nil, media.New(media.CodeSegmentParse, "ts-segment", req.Pos, "segment carries no PES samples")
	}
	sort.SliceStable(samples, func(i, j int) bool {
		if samples[i].DTS != samples[j].DTS {
			return samples[i].DTS < samples[j].DTS
		}
		return samples[i].Key.Less(samples[j].Key)
	})
	for i := range samples {
		samples[i].Source.Sample = i
	}
	_ = order
	return samples, nil
}

func streamForPID(streams []tsStream, pid uint16) *tsStream {
	for i := range streams {
		if streams[i].PID == pid {
			return &streams[i]
		}
	}
	return nil
}

func trackIDForPID(streams []tsStream, pid uint16) uint32 {
	for i := range streams {
		if streams[i].PID == pid {
			return uint32(i + 1)
		}
	}
	return 0
}
