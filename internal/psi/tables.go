package psi

import (
	"encoding/binary"
	"fmt"
)

// PATProgram maps a program number to its PMT PID.
type PATProgram struct {
	ProgramNumber uint16
	PMTPID        uint16
}

// PAT is a parsed Program Association Table.
type PAT struct {
	TransportStreamID uint16
	Version           uint8
	CurrentNext       bool
	Programs          []PATProgram
}

// PMTStream describes one elementary stream in a PMT.
type PMTStream struct {
	StreamType    uint8
	ElementaryPID uint16
}

// PMT is a parsed Program Map Table.
type PMT struct {
	ProgramNumber uint16
	PMTPID        uint16
	Version       uint8
	CurrentNext   bool
	PCRPID        uint16
	Streams       []PMTStream
}

// Service is the subset of an SDT service used by the report.
type Service struct {
	ServiceID uint16
	Name      string
	Provider  string
}

// SDT is a parsed Service Description Table.
type SDT struct {
	TransportStreamID uint16
	Version           uint8
	CurrentNext       bool
	Services          []Service
}

// ParsePAT parses a CRC-valid PAT section.
func ParsePAT(sec Section) (*PAT, error) {
	if len(sec.Body) < 2 || len(sec.Body)%4 != 2 {
		return nil, fmt.Errorf("PAT body length %d", len(sec.Body))
	}
	p := &PAT{
		TransportStreamID: binary.BigEndian.Uint16(sec.Body[:2]),
		Version:           sec.Version,
		CurrentNext:       sec.CurrentNext,
	}
	for i := 2; i+4 <= len(sec.Body); i += 4 {
		p.Programs = append(p.Programs, PATProgram{
			ProgramNumber: binary.BigEndian.Uint16(sec.Body[i : i+2]),
			PMTPID:        binary.BigEndian.Uint16(sec.Body[i+2:i+4]) & 0x1fff,
		})
	}
	return p, nil
}

// ParsePMT parses a CRC-valid PMT section.
func ParsePMT(sec Section) (*PMT, error) {
	if len(sec.Raw) < 16 || len(sec.Body) < 4 {
		return nil, fmt.Errorf("PMT section too short")
	}
	p := &PMT{
		ProgramNumber: binary.BigEndian.Uint16(sec.Raw[3:5]),
		PMTPID:        sec.PID,
		Version:       sec.Version,
		CurrentNext:   sec.CurrentNext,
		PCRPID:        binary.BigEndian.Uint16(sec.Body[:2]) & 0x1fff,
	}
	infoLen := int(binary.BigEndian.Uint16(sec.Body[2:4]) & 0x0fff)
	pos := 4 + infoLen
	if pos > len(sec.Body) {
		return nil, fmt.Errorf("PMT program_info_length exceeds section")
	}
	for pos+5 <= len(sec.Body) {
		streamType := sec.Body[pos]
		pid := binary.BigEndian.Uint16(sec.Body[pos+1:pos+3]) & 0x1fff
		esLen := int(binary.BigEndian.Uint16(sec.Body[pos+3:pos+5]) & 0x0fff)
		pos += 5
		if pos+esLen > len(sec.Body) {
			return nil, fmt.Errorf("PMT ES_info_length exceeds section")
		}
		p.Streams = append(p.Streams, PMTStream{StreamType: streamType, ElementaryPID: pid})
		pos += esLen
	}
	if pos != len(sec.Body) {
		return nil, fmt.Errorf("PMT has trailing bytes")
	}
	return p, nil
}

// ParseSDT parses the services and names carried by an SDT section.
func ParseSDT(sec Section) (*SDT, error) {
	if len(sec.Body) < 3 {
		return nil, fmt.Errorf("SDT body too short")
	}
	p := &SDT{
		TransportStreamID: binary.BigEndian.Uint16(sec.Raw[3:5]),
		Version:           sec.Version,
		CurrentNext:       sec.CurrentNext,
	}
	pos := 3
	for pos+5 <= len(sec.Body) {
		serviceID := binary.BigEndian.Uint16(sec.Body[pos : pos+2])
		descLen := int(binary.BigEndian.Uint16(sec.Body[pos+3:pos+5]) & 0x0fff)
		pos += 5
		if pos+descLen > len(sec.Body) {
			return nil, fmt.Errorf("SDT descriptor length exceeds section")
		}
		svc := Service{ServiceID: serviceID}
		parseServiceDescriptors(sec.Body[pos:pos+descLen], &svc)
		p.Services = append(p.Services, svc)
		pos += descLen
	}
	if pos != len(sec.Body) {
		return nil, fmt.Errorf("SDT has trailing bytes")
	}
	return p, nil
}

func parseServiceDescriptors(bs []byte, svc *Service) {
	for len(bs) >= 2 {
		tag, l := bs[0], int(bs[1])
		if 2+l > len(bs) {
			return
		}
		if tag == 0x48 {
			d := bs[2 : 2+l]
			if len(d) < 1 {
				return
			}
			pl := int(d[0])
			if 1+pl <= len(d) {
				svc.Provider = string(d[1 : 1+pl])
			}
			if 1+pl < len(d) {
				sl := int(d[1+pl])
				if 2+pl+sl <= len(d) {
					svc.Name = string(d[2+pl : 2+pl+sl])
				}
			}
		}
		bs = bs[2+l:]
	}
}

// TableName returns a stable short name for a PSI table id.
func TableName(id uint8) string {
	switch id {
	case TableIDPAT:
		return "PAT"
	case TableIDPMT:
		return "PMT"
	case TableIDSDT:
		return "SDT"
	default:
		return fmt.Sprintf("table_0x%02x", id)
	}
}
