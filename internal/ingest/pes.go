package ingest

import (
	"mediad/internal/mediaerr"
)

// Flush returns a complete unbounded PES builder at end of input. Bounded PES
// packets are emitted as soon as their declared length is received.
func (s *Scanner) Flush() []Record {
	var out []Record
	for pid, b := range s.pes {
		if b != nil && b.expected < 0 && len(b.data) > b.header {
			rec := b.rec
			rec.Payload = append([]byte(nil), b.data[b.header:]...)
			if pcr := s.pcrs[pid]; pcr != nil {
				pcrCopy := *pcr
				rec.PCR = &pcrCopy
			}
			out = append(out, rec)
		}
		delete(s.pes, pid)
	}
	return out
}

func (s *Scanner) appendPES(pid uint16, pusi bool, payload []byte, off int64, packetIndex int) (Record, bool, error) {
	if pusi {
		if len(payload) < 9 || payload[0] != 0 || payload[1] != 0 || payload[2] != 1 {
			return Record{}, false, mediaerr.New(mediaerr.KindCorruptPayload, off, pid, true, "PES start code missing")
		}
		streamID := payload[3]
		pesLen := int(uint16(payload[4])<<8 | uint16(payload[5]))
		flags := payload[7]
		headerLen := 9 + int(payload[8])
		if headerLen > len(payload) {
			return Record{}, false, mediaerr.New(mediaerr.KindCorruptPayload, off, pid, true, "PES header length %d exceeds packet", headerLen)
		}
		rec := Record{Offset: off, PacketIndex: packetIndex, PID: pid, StreamID: streamID}
		if flags&0x80 != 0 {
			pts, err := parseStamp(payload[9:14], off, pid, "PTS")
			if err != nil {
				return Record{}, false, err
			}
			rec.PTS = Stamp{Value: pts, Valid: true}
		}
		if flags&0xc0 == 0xc0 {
			if len(payload) < 19 {
				return Record{}, false, mediaerr.New(mediaerr.KindCorruptPayload, off, pid, true, "short PES DTS")
			}
			dts, err := parseStamp(payload[14:19], off, pid, "DTS")
			if err != nil {
				return Record{}, false, err
			}
			rec.DTS = Stamp{Value: dts, Valid: true}
		}
		if pcr := s.pcrs[pid]; pcr != nil {
			pcrCopy := *pcr
			rec.PCR = &pcrCopy
		}

		expected := -1
		if pesLen > 0 {
			expected = 6 + pesLen
			if expected > s.MaxPES {
				return Record{}, false, mediaerr.New(mediaerr.KindBufferLimit, off, pid, false, "PES size %d exceeds bound %d", expected, s.MaxPES)
			}
		}
		b := &pesBuilder{rec: rec, expected: expected, header: headerLen}
		b.data = append(b.data, payload...)
		if expected > 0 && len(b.data) >= expected {
			return b.emit(pid, s.pcrs)
		}
		if len(b.data) > s.MaxPES {
			return Record{}, false, mediaerr.New(mediaerr.KindBufferLimit, off, pid, false, "PES buffer exceeded bound")
		}
		s.pes[pid] = b
		return Record{}, false, nil
	}

	b := s.pes[pid]
	if b == nil {
		return Record{}, false, mediaerr.New(mediaerr.KindCorruptPayload, off, pid, true, "continuation PES packet without PUSI")
	}
	b.data = append(b.data, payload...)
	if len(b.data) > s.MaxPES {
		delete(s.pes, pid)
		return Record{}, false, mediaerr.New(mediaerr.KindBufferLimit, off, pid, false, "PES buffer exceeded bound")
	}
	if b.expected > 0 && len(b.data) >= b.expected {
		return b.emit(pid, s.pcrs)
	}
	return Record{}, false, nil
}

func (b *pesBuilder) emit(pid uint16, pcrs map[uint16]*PCR) (Record, bool, error) {
	end := len(b.data)
	if b.expected > 0 && b.expected < end {
		end = b.expected
	}
	if b.header > end {
		return Record{}, false, mediaerr.New(mediaerr.KindCorruptPayload, b.rec.Offset, pid, true, "PES header beyond declared length")
	}
	rec := b.rec
	rec.Payload = append([]byte(nil), b.data[b.header:end]...)
	if pcr := pcrs[pid]; pcr != nil {
		pcrCopy := *pcr
		rec.PCR = &pcrCopy
	}
	return rec, true, nil
}

func parseStamp(b []byte, off int64, pid uint16, name string) (uint64, error) {
	if len(b) < 5 {
		return 0, mediaerr.New(mediaerr.KindCorruptPayload, off, pid, true, "%s stamp truncated", name)
	}
	if b[0]&0x01 == 0 || b[2]&0x01 == 0 || b[4]&0x01 == 0 {
		return 0, mediaerr.New(mediaerr.KindCorruptPayload, off, pid, true, "%s marker bits invalid", name)
	}
	v := uint64(b[0]&0x0e)<<29 |
		uint64(b[1])<<22 |
		uint64(b[2]&0xfe)<<14 |
		uint64(b[3])<<7 |
		uint64(b[4])>>1
	if v >= 1<<33 {
		return 0, mediaerr.New(mediaerr.KindTimestampOverflow, off, pid, true, "%s exceeds 33 bits", name)
	}
	return v, nil
}

// ContainsIDR reports whether an H.264 Annex-B payload contains an IDR NAL.
func ContainsIDR(payload []byte) bool {
	for i := 0; i+4 < len(payload); i++ {
		if payload[i] == 0 && payload[i+1] == 0 && payload[i+2] == 1 {
			nal := payload[i+3] & 0x1f
			return nal == 5
		}
		if i+4 < len(payload) && payload[i] == 0 && payload[i+1] == 0 && payload[i+2] == 0 && payload[i+3] == 1 {
			if i+4 < len(payload) && payload[i+4]&0x1f == 5 {
				return true
			}
		}
	}
	return false
}
