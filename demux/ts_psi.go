package demux

import (
	"sort"

	"fieldpost/internal/core"
)

// parsePAT returns PMT PIDs from a PAT section carried in one TS payload.
func parsePAT(payload []byte) []uint16 {
	sec := psiSection(payload)
	if len(sec) < 8 || sec[0] != 0x00 {
		return nil
	}
	end := psiSectionEnd(sec)
	if end <= 0 {
		return nil
	}
	var pmt []uint16
	for off := 8; off+4 <= end-4; off += 4 {
		program := uint16(sec[off])<<8 | uint16(sec[off+1])
		pid := uint16(sec[off+2]&0x1f)<<8 | uint16(sec[off+3])
		if program != 0 && pid != 0x1fff {
			pmt = append(pmt, pid)
		}
	}
	return pmt
}

// parsePMT maps AVC/AAC elementary PIDs from a PMT section.
func parsePMT(payload []byte) map[uint16]core.Track {
	sec := psiSection(payload)
	streams := map[uint16]core.Track{}
	if len(sec) < 12 || sec[0] != 0x02 {
		return streams
	}
	end := psiSectionEnd(sec)
	if end <= 12 {
		return streams
	}
	infoLen := int(uint16(sec[10]&0x0f)<<8 | uint16(sec[11]))
	off := 12 + infoLen
	for off+5 <= end-4 {
		streamType := sec[off]
		pid := uint16(sec[off+1]&0x1f)<<8 | uint16(sec[off+2])
		esLen := int(uint16(sec[off+3]&0x0f)<<8 | uint16(sec[off+4]))
		switch streamType {
		case 0x1b, 0x24: // AVC
			streams[pid] = core.Track{ID: uint32(pid), Kind: core.TrackVideo, Codec: "avc1", Timescale: 90000}
		case 0x0f, 0x11: // AAC ADTS / LATM
			streams[pid] = core.Track{ID: uint32(pid), Kind: core.TrackAudio, Codec: "mp4a", Timescale: 90000, Channels: 2}
		}
		off += 5 + esLen
	}
	return streams
}

func psiSection(payload []byte) []byte {
	if len(payload) == 0 {
		return nil
	}
	pointer := int(payload[0])
	start := 1 + pointer
	if start >= len(payload) {
		return nil
	}
	return payload[start:]
}

func psiSectionEnd(sec []byte) int {
	if len(sec) < 3 {
		return -1
	}
	n := int(uint16(sec[1]&0x0f)<<8|uint16(sec[2])) + 3
	if n > len(sec) {
		return len(sec)
	}
	return n
}

func pesStreamKind(pes []byte) core.TrackKind {
	if len(pes) >= 4 {
		sid := pes[3]
		if sid >= 0xc0 && sid <= 0xdf {
			return core.TrackAudio
		}
		if sid >= 0xe0 && sid <= 0xef {
			return core.TrackVideo
		}
	}
	return ""
}

func tsTracks(kinds map[uint16]core.TrackKind, psi map[uint16]core.Track) []core.Track {
	ids := make([]uint16, 0, len(kinds))
	for id := range kinds {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var out []core.Track
	for _, id := range ids {
		if tr, ok := psi[id]; ok {
			out = append(out, tr)
			continue
		}
		kind := kinds[id]
		tr := core.Track{ID: uint32(id), Kind: kind, Timescale: 90000}
		if kind == core.TrackAudio {
			tr.Codec = "mp4a"
			tr.Channels = 2
		} else {
			tr.Codec = "avc1"
			tr.Width = 1280
			tr.Height = 720
		}
		out = append(out, tr)
	}
	return out
}
