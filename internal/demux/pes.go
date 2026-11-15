package demux

import (
	"bytes"
	"context"

	"github.com/asticode/go-astits"

	"tsprobe/internal/psi"
)

func parsePESData(payload []byte) (int64, bool) {
	pts, _, ok := parsePESHeader(payload)
	return pts, ok
}

func parsePESHeader(payload []byte) (pts int64, dataStart int, ok bool) {
	if len(payload) < 9 || payload[0] != 0 || payload[1] != 0 || payload[2] != 1 {
		return 0, 0, false
	}
	flags := payload[7]
	dataStart = 9 + int(payload[8])
	if dataStart > len(payload) {
		return 0, 0, false
	}
	if flags&0x80 == 0 || len(payload) < 14 || payload[9]&0x01 == 0 || payload[11]&0x01 == 0 || payload[13]&0x01 == 0 {
		return 0, dataStart, false
	}
	pts = int64(payload[9]&0x0e)<<29 |
		int64(payload[10])<<22 |
		int64(payload[11]&0xfe)<<14 |
		int64(payload[12])<<7 |
		int64(payload[13]>>1)
	return pts, dataStart, true
}

func inspectKeyframe(payload []byte, ref psi.StreamRef, idx uint64, off int64) (Keyframe, bool) {
	pts, data, ok := parseKeyframePES(ref.PID, payload)
	if !ok {
		manualPTS, dataStart, manualOK := parsePESHeader(payload)
		if !manualOK || dataStart >= len(payload) {
			return Keyframe{}, false
		}
		pts, data = manualPTS, payload[dataStart:]
	}
	if !hasRandomAccessNAL(data, ref.StreamType) {
		return Keyframe{}, false
	}
	return Keyframe{
		PacketIndex: idx, ByteOffset: off, PID: ref.PID, ProgramNumber: ref.ProgramNumber,
		StreamType: ref.StreamType, PTSMillis: pts / 90,
	}, true
}

func parseKeyframePES(pid uint16, payload []byte) (int64, []byte, bool) {
	raw := make([]byte, 188)
	raw[0] = 0x47
	raw[1] = 0x40 | byte(pid>>8)&0x1f
	raw[2] = byte(pid)
	if len(payload) < 184 {
		raw[3] = 0x30
		raw[4] = byte(183 - len(payload))
		raw[5] = 0
		copy(raw[188-len(payload):], payload)
	} else {
		raw[3] = 0x10
		copy(raw[4:], payload)
	}
	dmx := astits.NewDemuxer(context.Background(), bytes.NewReader(raw), astits.DemuxerOptPacketSize(188))
	data, err := dmx.NextData()
	if err != nil || data == nil || data.PES == nil || data.PES.Header == nil || data.PES.Header.OptionalHeader == nil {
		return 0, nil, false
	}
	pts := data.PES.Header.OptionalHeader.PTS
	if pts == nil {
		return 0, data.PES.Data, false
	}
	return pts.Base, data.PES.Data, true
}

func hasRandomAccessNAL(data []byte, streamType uint8) bool {
	limit := len(data) - 4
	if limit > 128 {
		limit = 128
	}
	for i := 0; i <= limit; i++ {
		if data[i] != 0 || data[i+1] != 0 || data[i+2] != 1 {
			continue
		}
		nal := data[i+3]
		if streamType == 0x24 {
			t := (nal >> 1) & 0x3f
			if t >= 16 && t <= 21 {
				return true
			}
			continue
		}
		if nal&0x1f == 5 {
			return true
		}
	}
	return false
}
