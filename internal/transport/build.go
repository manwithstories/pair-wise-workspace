package transport

import (
	"errors"
	"fmt"
)

// PacketOptions are the deterministic fields used to construct fixture TS.
type PacketOptions struct {
	PID     uint16
	PUSI    bool
	CC      uint8
	PCR     *uint64
	Payload []byte
}

// BuildPacket constructs one padded 188-byte TS packet. It is used by the
// in-repo fixture generator and round-trip tests; no external media is read.
func BuildPacket(o PacketOptions) ([]byte, error) {
	if o.PID > 0x1fff {
		return nil, fmt.Errorf("PID %d exceeds 13 bits", o.PID)
	}
	if o.CC > 15 {
		return nil, fmt.Errorf("CC %d exceeds 4 bits", o.CC)
	}
	raw := make([]byte, PacketSize)
	raw[0] = SyncByte
	raw[1] = byte((o.PID >> 8) & 0x1f)
	raw[2] = byte(o.PID)
	if o.PUSI {
		raw[1] |= 0x40
	}
	raw[3] = (o.CC & 0x0f)

	hasPCR := o.PCR != nil
	if hasPCR {
		if *o.PCR >= 1<<33 {
			return nil, errors.New("PCR base exceeds 33 bits")
		}
		// adaptation_field_length=7: flags byte plus six PCR bytes.
		raw[3] |= 0x30 // adaptation + payload
		raw[4] = 7
		raw[5] = 0x10 // PCR flag
		pcr := *o.PCR
		raw[6] = byte(pcr >> 25)
		raw[7] = byte(pcr >> 17)
		raw[8] = byte(pcr >> 9)
		raw[9] = byte(pcr >> 1)
		raw[10] = byte(pcr<<7) & 0x80
		raw[11] = 0x7e // six reserved one-bits and zero extension
		copy(raw[12:], ffPadding)
		maxPayload := PacketSize - 12
		if len(o.Payload) > maxPayload {
			return nil, fmt.Errorf("PCR payload %d exceeds %d", len(o.Payload), maxPayload)
		}
		copy(raw[12:], o.Payload)
		// Remaining bytes are already 0xff padding (legal payload stuffing for
		// our synthetic PES/PSI lengths).
		for i := 12 + len(o.Payload); i < PacketSize; i++ {
			raw[i] = 0xff
		}
	} else {
		raw[3] |= 0x10 // payload only
		if len(o.Payload) > PacketSize-4 {
			return nil, fmt.Errorf("payload %d exceeds %d", len(o.Payload), PacketSize-4)
		}
		copy(raw[4:], o.Payload)
		for i := 4 + len(o.Payload); i < PacketSize; i++ {
			raw[i] = 0xff
		}
	}
	return raw, nil
}

var ffPadding = make([]byte, PacketSize)

func init() {
	for i := range ffPadding {
		ffPadding[i] = 0xff
	}
}
