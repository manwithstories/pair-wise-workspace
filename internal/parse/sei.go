package parse

import (
	"encoding/binary"
	"fmt"
	"mediarun/internal/model"
)

// NAL units are length-prefixed with a 4-byte big-endian length.
func splitNALUnits(sample []byte) ([][]byte, error) {
	var out [][]byte
	pos := 0
	for pos < len(sample) {
		if len(sample)-pos < 4 {
			return nil, model.Classify(model.ClassCorrupt,
				fmt.Errorf("%w: nal length prefix truncation", model.ErrTruncated))
		}
		n := int(binary.BigEndian.Uint32(sample[pos : pos+4]))
		if n < 0 || n > MaxSEIPayload || pos+4+n > len(sample) {
			return nil, model.Classify(model.ClassCorrupt,
				fmt.Errorf("%w: nal length %d of %d remaining", model.ErrTruncated, n, len(sample)-pos-4))
		}
		out = append(out, sample[pos+4:pos+4+n])
		pos += 4 + n
	}
	return out, nil
}

// seiTypeUserDataUnregistered is SEI payload type 5; its NAL unit type is 6.
const seiTypeUserDataUnregistered = 5

// ParseSEI extracts unregistered user-data SEI messages from an
// AVC length-prefixed sample.
func ParseSEI(sample []byte) ([]SEIEvent, error) {
	nals, err := splitNALUnits(sample)
	if err != nil {
		return nil, err
	}
	var out []SEIEvent
	for _, nal := range nals {
		if len(nal) == 0 || nal[0]&0x1F != 6 {
			continue
		}
		rbsp := removeEmulationPrevention(nal[1:])
		pos := 0
		index := 0
		for pos < len(rbsp) {
			// rbsp trailing bits: a lone 0x80 terminates the SEI.
			if pos == len(rbsp)-1 && rbsp[pos] == 0x80 {
				pos++
				break
			}
			payloadType := 0
			for pos < len(rbsp) && rbsp[pos] == 0xFF {
				payloadType += 255
				pos++
			}
			if pos >= len(rbsp) {
				return nil, model.Classify(model.ClassCorrupt,
					fmt.Errorf("%w: sei payload type truncated", model.ErrTruncated))
			}
			payloadType += int(rbsp[pos])
			pos++
			payloadSize := 0
			for pos < len(rbsp) && rbsp[pos] == 0xFF {
				payloadSize += 255
				pos++
			}
			if pos >= len(rbsp) {
				return nil, model.Classify(model.ClassCorrupt,
					fmt.Errorf("%w: sei payload size truncated", model.ErrTruncated))
			}
			payloadSize += int(rbsp[pos])
			pos++
			if payloadSize > MaxSEIPayload || pos+payloadSize > len(rbsp) {
				return nil, model.Classify(model.ClassCorrupt,
					fmt.Errorf("%w: sei payload %d bytes", model.ErrLimit, payloadSize))
			}
			payload := rbsp[pos : pos+payloadSize]
			pos += payloadSize
			if payloadType != seiTypeUserDataUnregistered {
				continue
			}
			if len(payload) < 16 {
				return nil, model.Classify(model.ClassCorrupt,
					fmt.Errorf("%w: sei user data shorter than uuid", model.ErrTruncated))
			}
			var uuid [16]byte
			copy(uuid[:], payload[:16])
			index++
			out = append(out, SEIEvent{
				ID:      fmt.Sprintf("sei-%d-%d", seiUUIDHash(uuid), index),
				UUID:    uuid,
				Payload: append([]byte(nil), payload[16:]...),
			})
		}
		if pos != len(rbsp) {
			return nil, model.Classify(model.ClassCorrupt,
				fmt.Errorf("%w: sei trailing %d bytes", model.ErrBadLength, len(rbsp)-pos))
		}
	}
	return out, nil
}
