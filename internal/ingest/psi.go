package ingest

import (
	"encoding/binary"

	"mediad/internal/mediaerr"
)

const (
	PATPID           = 0x0000
	StreamH264 uint8 = 0x1b
	StreamAAC  uint8 = 0x0f
)

// PAT is a parsed Program Association Table.
type PAT struct {
	TransportStreamID uint16
	Version           uint8
	Programs          map[uint16]uint16 // program number -> PMT PID
}

// PMT is a parsed Program Map Table.
type PMT struct {
	ProgramNumber uint16
	Version       uint8
	PCRPID        uint16
	Streams       map[uint16]uint8 // elementary PID -> stream type
}

type psiAssembly struct {
	pid     uint16
	buf     []byte
	version uint8
	haveVer bool
}

// PSIRegistry retains only the last good version of each table.
type PSIRegistry struct {
	PAT       *PAT
	PMTs      map[uint16]*PMT
	versions  map[uint16]uint8
	assembler map[uint16]*psiAssembly
}

// NewPSIRegistry creates an empty PSI registry.
func NewPSIRegistry() *PSIRegistry {
	return &PSIRegistry{
		PMTs:      make(map[uint16]*PMT),
		versions:  make(map[uint16]uint8),
		assembler: make(map[uint16]*psiAssembly),
	}
}

// IsPSI reports whether pid is PAT or a current/observed PMT PID.
func (r *PSIRegistry) IsPSI(pid uint16) bool {
	if pid == PATPID {
		return true
	}
	if _, ok := r.PMTs[pid]; ok {
		return true
	}
	if a, ok := r.assembler[pid]; ok && len(a.buf) > 0 {
		return true
	}
	return false
}

// HandlePacket feeds one TS payload to section reassembly.
func (r *PSIRegistry) HandlePacket(pid uint16, pusi bool, payload []byte, offset int64) error {
	if len(payload) == 0 {
		return mediaerr.New(mediaerr.KindInvalidPacket, offset, pid, true, "empty PSI payload")
	}
	a := r.assembler[pid]
	if a == nil {
		a = &psiAssembly{pid: pid}
		r.assembler[pid] = a
	}

	start := 0
	if pusi {
		pointer := int(payload[0])
		start = 1 + pointer
		if start > len(payload) {
			return mediaerr.New(mediaerr.KindInvalidPacket, offset, pid, true, "PSI pointer %d exceeds payload", pointer)
		}
		a.buf = append([]byte(nil), payload[start:]...)
		a.haveVer = false
	} else {
		if len(a.buf) == 0 {
			return mediaerr.New(mediaerr.KindInvalidPacket, offset, pid, true, "continuation PSI packet without section start")
		}
		a.buf = append(a.buf, payload...)
	}
	if len(a.buf) < 3 {
		return nil
	}
	sectionLen := int(binary.BigEndian.Uint16(a.buf[1:3]) & 0x0fff)
	total := sectionLen + 3
	if total > 4096 {
		return mediaerr.New(mediaerr.KindBufferLimit, offset, pid, false, "PSI section length %d exceeds 4096", total)
	}
	if len(a.buf) < total {
		return nil
	}
	section := append([]byte(nil), a.buf[:total]...)
	a.buf = a.buf[total:]
	return r.applySection(a, pid, section, offset)
}

func (r *PSIRegistry) applySection(a *psiAssembly, pid uint16, sec []byte, offset int64) error {
	if len(sec) < 9 {
		return mediaerr.New(mediaerr.KindInvalidPacket, offset, pid, true, "PSI section too short")
	}
	gotCRC := binary.BigEndian.Uint32(sec[len(sec)-4:])
	if wantCRC := MPEGCRC32(sec[:len(sec)-4]); gotCRC != wantCRC {
		return mediaerr.New(mediaerr.KindChecksumMismatch, offset, pid, true,
			"bad PSI CRC: got %08x want %08x; retaining previous table", gotCRC, wantCRC)
	}
	version := (sec[5] >> 1) & 0x1f
	if a.haveVer && a.version != version {
		return mediaerr.New(mediaerr.KindInvalidPacket, offset, pid, true,
			"PSI version mismatch within assembled table: %d != %d", version, a.version)
	}
	a.haveVer = true
	a.version = version
	currentNext := sec[5]&0x01 == 1
	if !currentNext {
		return nil // next table is signaled but not active yet.
	}
	secNum, lastNum := sec[6], sec[7]
	if secNum > lastNum {
		return mediaerr.New(mediaerr.KindInvalidPacket, offset, pid, true, "PSI section_number %d > last %d", secNum, lastNum)
	}

	switch sec[0] {
	case 0x00:
		pat, err := parsePAT(sec, version)
		if err != nil {
			return mediaerr.Wrap(mediaerr.KindInvalidPacket, offset, pid, true, err)
		}
		r.PAT = pat
		r.versions[pid] = version
		for _, pmtPID := range pat.Programs {
			if r.PMTs[pmtPID] == nil {
				r.PMTs[pmtPID] = &PMT{Version: 255, Streams: map[uint16]uint8{}}
			}
		}
	case 0x02:
		pmt, err := parsePMT(sec, version)
		if err != nil {
			return mediaerr.Wrap(mediaerr.KindInvalidPacket, offset, pid, true, err)
		}
		r.PMTs[pid] = pmt
		r.versions[pid] = version
	default:
		return mediaerr.New(mediaerr.KindUnsupportedCodec, offset, pid, true, "unsupported PSI table id 0x%02x", sec[0])
	}
	return nil
}

func parsePAT(sec []byte, version uint8) (*PAT, error) {
	pat := &PAT{
		TransportStreamID: binary.BigEndian.Uint16(sec[3:5]),
		Version:           version,
		Programs:          map[uint16]uint16{},
	}
	end := len(sec) - 4
	for i := 8; i+4 <= end; i += 4 {
		num := binary.BigEndian.Uint16(sec[i : i+2])
		pid := binary.BigEndian.Uint16(sec[i+2:i+4]) & 0x1fff
		if num != 0 {
			pat.Programs[num] = pid
		}
	}
	if len(pat.Programs) == 0 {
		return nil, mediaerr.New(mediaerr.KindInvalidPacket, -1, PATPID, true, "PAT has no program")
	}
	return pat, nil
}

func parsePMT(sec []byte, version uint8) (*PMT, error) {
	pmt := &PMT{
		ProgramNumber: binary.BigEndian.Uint16(sec[3:5]),
		Version:       version,
		PCRPID:        binary.BigEndian.Uint16(sec[8:10]) & 0x1fff,
		Streams:       map[uint16]uint8{},
	}
	piLen := int(binary.BigEndian.Uint16(sec[10:12]) & 0x0fff)
	p := 12 + piLen
	end := len(sec) - 4
	for p+5 <= end {
		streamType := sec[p]
		pid := binary.BigEndian.Uint16(sec[p+1:p+3]) & 0x1fff
		esLen := int(binary.BigEndian.Uint16(sec[p+3:p+5]) & 0x0fff)
		pmt.Streams[pid] = streamType
		p += 5 + esLen
	}
	if len(pmt.Streams) == 0 {
		return nil, mediaerr.New(mediaerr.KindInvalidPacket, -1, 0, true, "PMT has no elementary streams")
	}
	return pmt, nil
}

var crcTable = func() [256]uint32 {
	var t [256]uint32
	for i := 0; i < 256; i++ {
		crc := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
		t[i] = crc
	}
	return t
}()

// MPEGCRC32 computes the MPEG-2 PSI CRC.
func MPEGCRC32(b []byte) uint32 {
	crc := uint32(0xffffffff)
	for _, v := range b {
		crc = (crc << 8) ^ crcTable[byte(crc>>24)^v]
	}
	return crc
}
