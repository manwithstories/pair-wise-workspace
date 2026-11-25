package ts

import (
	"fmt"

	"cinepipe/internal/fault"
)

// PSI table identifiers relevant to a TS program.
const (
	TIDPat = 0x00
	TIDPmt = 0x02
)

// Stream types recognised in a PMT for the demux paths cinepipe supports.
const (
	StreamTypeH264 = 0x1B
	StreamTypeHEVC = 0x24
	StreamTypeAAC  = 0x0F
	StreamTypeMP3  = 0x03
	StreamTypeAC3  = 0x81
	StreamTypeEAC3 = 0x87
)

// maxSectionLen is the largest legal PSI section total length (3-byte header +
// section_length body), per ISO/IEC 13818-1.
const maxSectionLen = 3 + 4093

// CRC32MPEG computes the CRC-32 used by MPEG-2 PSI sections: polynomial
// 0x04C11DB7, MSB-first, initial value all ones, no final xor.
func CRC32MPEG(data []byte) uint32 {
	var crc uint32 = 0xFFFFFFFF
	for _, b := range data {
		crc ^= uint32(b) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// Section is a parsed PSI table carried in one or more packets.
type Section struct {
	TableID  uint8
	Ext      uint16 // table_id_extension: transport_stream_id (PAT) or program_number (PMT)
	Version  uint8
	Current  bool
	Section  uint8
	LastSec  uint8
	Bytes    []byte // section payload after the 8-byte header, excluding the CRC
	Offset   int64  // byte offset of the packet that began the section
	Packets  int64  // number of packets that contributed
	Spanned  bool   // true when assembly crossed packet boundaries
	Complete bool
	CRCValid bool
}

// PAT is the program association table: program number to PMT PID.
type PAT struct {
	TransportStreamID uint16
	Version           uint8
	Programs          []ProgramEntry
	Section           *Section
}

// ProgramEntry ties a program number to its PMT PID.
type ProgramEntry struct {
	Program    uint16
	PMTPID     uint16
	NetworkPID uint16 // program number 0 denotes the network PID
}

// PMT is the program map table for one program.
type PMT struct {
	Program uint16
	Version uint8
	PCRPID  uint16
	Streams []StreamEntry
	Section *Section
}

// StreamEntry is one elementary stream declared in a PMT.
type StreamEntry struct {
	StreamType  uint8
	PID         uint16
	ESInfoLen   uint16
	ESInfo      []byte
	HasLanguage string
}

// IsVideo reports whether the stream type is a video codec cinepipe indexes.
func (s StreamEntry) IsVideo() bool {
	return s.StreamType == StreamTypeH264 || s.StreamType == StreamTypeHEVC
}

// IsAudio reports whether the stream type is an audio codec cinepipe indexes.
func (s StreamEntry) IsAudio() bool {
	switch s.StreamType {
	case StreamTypeAAC, StreamTypeMP3, StreamTypeAC3, StreamTypeEAC3:
		return true
	}
	return false
}

// partialSection is the in-progress assembly of one PID's section.
type partialSection struct {
	bytes    []byte
	start    int64
	packets  int64
	spanned  bool
	complete bool
	consumed int // bytes taken from the most recent payload when completed
	appends  int
}

// SectionAssembler reassembles PSI sections from packet payloads. It handles
// the pointer_field, sections that span packets, and stuffing bytes that pad
// the last packet of a table. Versions are tracked per (PID, table, extension)
// so a mid-stream version switch is reported rather than silently overwritten.
type SectionAssembler struct {
	partial  map[uint16]*partialSection
	versions map[sectionKey]uint8
}

type sectionKey struct {
	pid uint16
	tid uint8
	ext uint16
}

// NewSectionAssembler returns an empty assembler.
func NewSectionAssembler() *SectionAssembler {
	return &SectionAssembler{
		partial:  make(map[uint16]*partialSection),
		versions: make(map[sectionKey]uint8),
	}
}

// Assemble consumes a packet payload and returns every complete section it
// yields. Diagnostics are located at the packet that contributed the bytes.
func (a *SectionAssembler) Assemble(p *Packet) ([]*Section, []fault.Error) {
	var out []*Section
	var errs []fault.Error
	if !p.HasPayload || p.PID == NullPID || len(p.Payload) == 0 {
		return nil, nil
	}
	data := p.Payload
	pos := 0
	if p.PUSI {
		ptr := int(data[0])
		if ptr > len(data)-1 {
			delete(a.partial, p.PID)
			errs = append(errs, p.Errorf(fault.KindPSI,
				fmt.Sprintf("pointer_field %d exceeds payload %d", ptr, len(data)-1)))
			return nil, errs
		}
		// Bytes before the pointer finish the previous section.
		if ptr > 0 {
			if part, ok := a.partial[p.PID]; ok {
				sect, ferr := a.consume(p, part, data[1:1+ptr])
				if sect != nil {
					out = append(out, sect)
				}
				if ferr != nil {
					errs = append(errs, *ferr)
				}
			}
		} else if part, ok := a.partial[p.PID]; ok {
			// A new section begins at once, so any buffered half-section can
			// never complete. Report it as a PSI structural loss rather than
			// letting it silently merge with the new table.
			errs = append(errs, fault.Error{Kind: fault.KindPSI, Offset: part.start, PID: p.PID,
				Detail: fmt.Sprintf("abandoned partial section after %d bytes", len(part.bytes))})
		}
		delete(a.partial, p.PID)
		pos = 1 + ptr
	}

	for pos < len(data) {
		if data[pos] == 0xFF {
			// Stuffing to the end of the packet; legal padding.
			break
		}
		if part, ok := a.partial[p.PID]; ok {
			before := len(part.bytes)
			sect, ferr := a.consume(p, part, data[pos:])
			if sect != nil {
				out = append(out, sect)
			}
			if ferr != nil {
				errs = append(errs, *ferr)
			}
			if !part.complete {
				return out, errs
			}
			used := part.consumed - before
			if used <= 0 || used > len(data)-pos {
				used = len(data) - pos
			}
			pos += used
			continue
		}
		rest := data[pos:]
		if len(rest) < 3 {
			part := &partialSection{bytes: append([]byte(nil), rest...),
				start: p.Offset, packets: 1, spanned: true, appends: 1}
			a.partial[p.PID] = part
			return out, errs
		}
		sectionLen := int(rest[1]&0x0F)<<8 | int(rest[2])
		total := 3 + sectionLen
		if total > maxSectionLen {
			errs = append(errs, p.Errorf(fault.KindPSI,
				fmt.Sprintf("section length %d exceeds 4093", sectionLen)))
			return out, errs
		}
		if len(rest) >= total {
			part := &partialSection{start: p.Offset, packets: 1}
			sect, ferr := a.consume(p, part, rest[:total])
			if sect != nil {
				out = append(out, sect)
			}
			if ferr != nil {
				errs = append(errs, *ferr)
			}
			pos += total
			continue
		}
		a.partial[p.PID] = &partialSection{bytes: append([]byte(nil), rest...),
			start: p.Offset, packets: 1, spanned: true, appends: 1}
		return out, errs
	}
	return out, errs
}

// consume merges bytes into a partial section until it completes or the input
// is exhausted. It sets part.complete and part.consumed when a section closes.
// A partial created from a fresh packet starts with packets=1, so increments
// here represent continuation packets only.
func (a *SectionAssembler) consume(p *Packet, part *partialSection, next []byte) (*Section, *fault.Error) {
	part.appends++
	if part.appends > 1 {
		part.packets++
	}
	part.bytes = append(part.bytes, next...)
	if len(part.bytes) < 3 {
		a.partial[p.PID] = part
		return nil, nil
	}
	sectionLen := int(part.bytes[1]&0x0F)<<8 | int(part.bytes[2])
	total := 3 + sectionLen
	if total > maxSectionLen {
		delete(a.partial, p.PID)
		e := fault.Error{Kind: fault.KindPSI, Offset: part.start, PID: p.PID,
			Detail: fmt.Sprintf("section length %d exceeds 4093", sectionLen)}
		return nil, &e
	}
	if len(part.bytes) < total {
		a.partial[p.PID] = part
		return nil, nil
	}
	part.complete = true
	part.consumed = total
	delete(a.partial, p.PID)
	return a.finish(p.PID, part.bytes[:total], part)
}

// finish validates and parses one complete section.
func (a *SectionAssembler) finish(pid uint16, raw []byte, part *partialSection) (*Section, *fault.Error) {
	if len(raw) < 3 {
		e := fault.Error{Kind: fault.KindPSI, Offset: part.start, PID: pid,
			Detail: "section shorter than header"}
		return nil, &e
	}
	sectionLen := int(raw[1]&0x0F)<<8 | int(raw[2])
	if 3+sectionLen > len(raw) {
		e := fault.Error{Kind: fault.KindPSI, Offset: part.start, PID: pid,
			Detail: "section incomplete at packet boundary"}
		return nil, &e
	}
	if 3+sectionLen < 8 {
		e := fault.Error{Kind: fault.KindPSI, Offset: part.start, PID: pid,
			Detail: fmt.Sprintf("section too short for header: %d", sectionLen)}
		return nil, &e
	}
	s := &Section{
		TableID:  raw[0],
		Ext:      uint16(raw[3])<<8 | uint16(raw[4]),
		Version:  (raw[5] >> 1) & 0x1F,
		Current:  raw[5]&0x01 != 0,
		Section:  raw[6],
		LastSec:  raw[7],
		Bytes:    raw[8 : 3+sectionLen-4],
		Offset:   part.start,
		Packets:  part.packets,
		Spanned:  part.spanned,
		Complete: true,
	}
	want := uint32(raw[3+sectionLen-4])<<24 | uint32(raw[3+sectionLen-3])<<16 |
		uint32(raw[3+sectionLen-2])<<8 | uint32(raw[3+sectionLen-1])
	s.CRCValid = CRC32MPEG(raw[:3+sectionLen-4]) == want
	if !s.CRCValid {
		e := fault.Error{Kind: fault.KindCRC, Offset: part.start, PID: pid,
			Detail: fmt.Sprintf("crc mismatch tid=0x%02x ext=0x%04x", s.TableID, s.Ext)}
		return s, &e
	}
	key := sectionKey{pid: pid, tid: s.TableID, ext: s.Ext}
	if prev, ok := a.versions[key]; ok && prev != s.Version {
		e := fault.Error{Kind: fault.KindPSI, Offset: part.start, PID: pid,
			Detail: fmt.Sprintf("version switch tid=0x%02x ext=0x%04x %d->%d",
				s.TableID, s.Ext, prev, s.Version)}
		a.versions[key] = s.Version
		return s, &e
	}
	a.versions[key] = s.Version
	return s, nil
}

// ParsePAT interprets a PAT section.
func ParsePAT(s *Section) (*PAT, error) {
	if s.TableID != TIDPat {
		return nil, fmt.Errorf("not a PAT: tid=0x%02x", s.TableID)
	}
	if len(s.Bytes)%4 != 0 {
		return nil, fmt.Errorf("PAT body length %d not a multiple of 4", len(s.Bytes))
	}
	pat := &PAT{TransportStreamID: s.Ext, Version: s.Version, Section: s}
	for i := 0; i+4 <= len(s.Bytes); i += 4 {
		prog := uint16(s.Bytes[i])<<8 | uint16(s.Bytes[i+1])
		pid := uint16(s.Bytes[i+2]&0x1F)<<8 | uint16(s.Bytes[i+3])
		if prog == 0 {
			pat.Programs = append(pat.Programs, ProgramEntry{NetworkPID: pid})
			continue
		}
		pat.Programs = append(pat.Programs, ProgramEntry{Program: prog, PMTPID: pid})
	}
	return pat, nil
}

// ParsePMT interprets a PMT section.
func ParsePMT(s *Section) (*PMT, error) {
	if s.TableID != TIDPmt {
		return nil, fmt.Errorf("not a PMT: tid=0x%02x", s.TableID)
	}
	if len(s.Bytes) < 4 {
		return nil, fmt.Errorf("PMT body too short: %d", len(s.Bytes))
	}
	pmt := &PMT{Program: s.Ext, Version: s.Version, Section: s}
	pmt.PCRPID = uint16(s.Bytes[0]&0x1F)<<8 | uint16(s.Bytes[1])
	progInfoLen := int(s.Bytes[2]&0x0F)<<8 | int(s.Bytes[3])
	pos := 4 + progInfoLen
	for pos+5 <= len(s.Bytes) {
		st := s.Bytes[pos]
		pid := uint16(s.Bytes[pos+1]&0x1F)<<8 | uint16(s.Bytes[pos+2])
		esLen := int(s.Bytes[pos+3]&0x0F)<<8 | int(s.Bytes[pos+4])
		end := pos + 5 + esLen
		if end > len(s.Bytes) {
			return nil, fmt.Errorf("PMT stream descriptor overruns section")
		}
		ent := StreamEntry{StreamType: st, PID: pid, ESInfoLen: uint16(esLen)}
		if esLen > 0 {
			ent.ESInfo = s.Bytes[pos+5 : end]
			if lang := findLanguageDescriptor(ent.ESInfo); lang != "" {
				ent.HasLanguage = lang
			}
		}
		pmt.Streams = append(pmt.Streams, ent)
		pos = end
	}
	return pmt, nil
}

// findLanguageDescriptor walks ES descriptors looking for an ISO 639 language
// tag (0x0A).
func findLanguageDescriptor(b []byte) string {
	for i := 0; i+2 <= len(b); {
		tag := b[i]
		l := int(b[i+1])
		if i+2+l > len(b) {
			return ""
		}
		if tag == 0x0A && l >= 3 {
			return string(b[i+2 : i+5])
		}
		i += 2 + l
	}
	return ""
}

// BuildSection assembles a single complete PSI section, appending the trailing
// CRC. It exists so tests and the sample generator can construct tables without
// duplicating the layout logic. The section_number and last_section_number
// fields are zero, which fits the single-section tables cinepipe builds.
func BuildSection(tableID uint8, ext uint16, version uint8, body []byte) []byte {
	total := 3 + 5 + len(body) + 4 // header 3, ext/version/section/last 5, body, CRC 4
	b := make([]byte, 0, total)
	b = append(b, tableID)
	b = append(b, 0xB0|uint8((total-3)>>8)&0x0F)
	b = append(b, uint8(total-3))
	b = append(b, uint8(ext>>8), uint8(ext))
	b = append(b, 0xC1|(version&0x1F)<<1)
	b = append(b, 0x00, 0x00) // section_number, last_section_number
	b = append(b, body...)
	crc := CRC32MPEG(b)
	b = append(b, uint8(crc>>24), uint8(crc>>16), uint8(crc>>8), uint8(crc))
	return b
}
