package psi

// Descriptor is a raw PSI descriptor, preserved as-is.
type Descriptor struct {
	Tag  byte   `json:"tag"`
	Data []byte `json:"data,omitempty"`
}

// parseDescriptors parses a descriptor loop with bounds protection.
func parseDescriptors(b []byte) []Descriptor {
	var out []Descriptor
	for len(b) >= 2 {
		l := int(b[1])
		if 2+l > len(b) {
			break // truncated descriptor: drop the tail
		}
		d := Descriptor{Tag: b[0]}
		if l > 0 {
			d.Data = append([]byte(nil), b[2:2+l]...)
		}
		out = append(out, d)
		b = b[2+l:]
	}
	return out
}

// sectionHeader parses the common PSI long-section header.
// raw includes the CRC. Returns false when malformed.
func sectionHeader(raw []byte) (id uint16, version byte, currentNext bool, section, lastSection byte, ok bool) {
	if len(raw) < 8 {
		return 0, 0, false, 0, 0, false
	}
	id = uint16(raw[3])<<8 | uint16(raw[4])
	version = (raw[5] >> 1) & 0x1F
	currentNext = raw[5]&0x01 != 0
	section = raw[6]
	lastSection = raw[7]
	return id, version, currentNext, section, lastSection, true
}

// PAT is a parsed program association table (possibly multi-section merged).
type PAT struct {
	TSID        uint16
	Version     byte
	CurrentNext bool
	NITPIDs     []uint16
	Programs    map[uint16]uint16 // program_number -> PMT PID
}

// ParsePATSection parses one PAT section (table_id 0x00).
func ParsePATSection(raw []byte) (*PAT, byte, byte, bool) {
	if len(raw) < 12 || raw[0] != 0x00 {
		return nil, 0, 0, false
	}
	id, version, cn, sec, last, ok := sectionHeader(raw)
	if !ok {
		return nil, 0, 0, false
	}
	pat := &PAT{TSID: id, Version: version, CurrentNext: cn, Programs: map[uint16]uint16{}}
	body := raw[8 : len(raw)-4]
	for len(body) >= 4 {
		prog := uint16(body[0])<<8 | uint16(body[1])
		pid := uint16(body[2]&0x1F)<<8 | uint16(body[3])
		if prog == 0 {
			pat.NITPIDs = append(pat.NITPIDs, pid)
		} else {
			pat.Programs[prog] = pid
		}
		body = body[4:]
	}
	_ = sec
	return pat, sec, last, true
}

// Stream is one ES entry of a PMT.
type Stream struct {
	StreamType  byte         `json:"stream_type"`
	PID         uint16       `json:"pid"`
	Descriptors []Descriptor `json:"descriptors,omitempty"`
}

// PMT is a parsed program map table.
type PMT struct {
	ProgramNumber uint16
	Version       byte
	CurrentNext   bool
	PCRPID        uint16
	Descriptors   []Descriptor
	Streams       []Stream
}

// ParsePMT parses one PMT section (table_id 0x02).
func ParsePMT(raw []byte) (*PMT, bool) {
	if len(raw) < 16 || raw[0] != 0x02 {
		return nil, false
	}
	id, version, cn, _, _, ok := sectionHeader(raw)
	if !ok {
		return nil, false
	}
	pmt := &PMT{ProgramNumber: id, Version: version, CurrentNext: cn}
	pmt.PCRPID = uint16(raw[8]&0x1F)<<8 | uint16(raw[9])
	pil := int(raw[10]&0x0F)<<8 | int(raw[11])
	end := len(raw) - 4
	if 12+pil > end {
		return nil, false
	}
	pmt.Descriptors = parseDescriptors(raw[12 : 12+pil])
	body := raw[12+pil : end]
	for len(body) >= 5 {
		st := body[0]
		pid := uint16(body[1]&0x1F)<<8 | uint16(body[2])
		eil := int(body[3]&0x0F)<<8 | int(body[4])
		if 5+eil > len(body) {
			break // truncated ES info: drop the tail
		}
		pmt.Streams = append(pmt.Streams, Stream{
			StreamType:  st,
			PID:         pid,
			Descriptors: parseDescriptors(body[5 : 5+eil]),
		})
		body = body[5+eil:]
	}
	return pmt, true
}

// StreamTypeName maps common stream_type values to names.
func StreamTypeName(t byte) string {
	switch t {
	case 0x01:
		return "MPEG-1 video"
	case 0x02:
		return "MPEG-2 video"
	case 0x03:
		return "MPEG-1 audio"
	case 0x04:
		return "MPEG-2 audio"
	case 0x0F:
		return "AAC ADTS"
	case 0x11:
		return "AAC LATM"
	case 0x1B:
		return "H.264 video"
	case 0x24:
		return "H.265 video"
	case 0x06:
		return "private PES"
	case 0x81:
		return "private (AC-3)"
	case 0x87:
		return "private (E-AC-3)"
	default:
		return "unknown"
	}
}
