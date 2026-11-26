package segment

import (
	"fmt"

	"aircheck/internal/media"
)

func parseTSPacket(artifact string, b []byte, off int64) (tsPacket, error) {
	if len(b) != tsPacketSize {
		return tsPacket{}, media.NewFault(media.FaultTruncated,
			media.SourcePos{Artifact: artifact, Offset: off}, "short transport packet")
	}
	if b[0] != tsSyncByte {
		return tsPacket{}, media.NewFault(media.FaultTruncated,
			media.SourcePos{Artifact: artifact, Offset: off},
			fmt.Sprintf("sync byte 0x%02x at transport packet boundary", b[0]))
	}
	p := tsPacket{offset: off, payload: b[4:]}
	p.pusi = b[1]&0x40 != 0
	p.pid = uint16(b[1]&0x1F)<<8 | uint16(b[2])
	p.cc = b[3] & 0x0F
	afc := (b[3] >> 4) & 0x03
	switch afc {
	case 0:
		return tsPacket{}, media.NewFault(media.FaultTruncated,
			media.SourcePos{Artifact: artifact, Offset: off}, "adaptation_field_control=0 is reserved")
	case 1:
		p.hasPayload = true
	case 2:
		p.hasPayload = false
	case 3:
		p.hasPayload = true
	}
	if afc >= 2 {
		afLen := int(b[4])
		if afLen+5 > tsPacketSize {
			return tsPacket{}, media.NewFault(media.FaultTruncated,
				media.SourcePos{Artifact: artifact, Offset: off},
				fmt.Sprintf("adaptation field length %d exceeds packet", afLen))
		}
		if afLen > 0 && b[5]&0x80 != 0 {
			p.discont = true
		}
		if !p.hasPayload {
			p.payload = nil
		} else if 5+afLen <= tsPacketSize {
			p.payload = b[5+afLen:]
		} else {
			p.payload = nil
		}
	}
	return p, nil
}

// parsePAT extracts the first PMT PID.
func parsePAT(payload []byte, pmtPID *uint16, seen *bool) error {
	ptr := int(payload[0])
	if 1+ptr >= len(payload) {
		return fmt.Errorf("PAT pointer_field %d beyond payload", ptr)
	}
	tab := payload[1+ptr:]
	if len(tab) < 8 || tab[0] != 0x00 {
		return fmt.Errorf("PAT table_id %d", tab[0])
	}
	slen := int(tab[1]&0x0F)<<8 | int(tab[2])
	if 3+slen > len(tab) || slen < 5 {
		return fmt.Errorf("PAT section length %d invalid", slen)
	}
	body := tab[8 : 3+slen-4]
	for i := 0; i+4 <= len(body); i += 4 {
		prog := uint16(body[i])<<8 | uint16(body[i+1])
		pid := uint16(body[i+2]&0x1F)<<8 | uint16(body[i+3])
		if prog != 0 {
			*pmtPID = pid
			*seen = true
			return nil
		}
	}
	return fmt.Errorf("PAT has no program entry")
}

// parsePMT maps elementary PIDs to track descriptors using stream_type.
func parsePMT(payload []byte, out map[uint16]Track) error {
	ptr := int(payload[0])
	if 1+ptr >= len(payload) {
		return fmt.Errorf("PMT pointer_field beyond payload")
	}
	tab := payload[1+ptr:]
	if len(tab) < 12 || tab[0] != 0x02 {
		return fmt.Errorf("PMT table_id %d", tab[0])
	}
	slen := int(tab[1]&0x0F)<<8 | int(tab[2])
	if 3+slen > len(tab) || slen < 13 {
		return fmt.Errorf("PMT section length %d invalid", slen)
	}
	pinfo := int(tab[10]&0x0F)<<8 | int(tab[11])
	body := 12 + pinfo
	end := 3 + slen - 4
	if body > end || end > len(tab) {
		return fmt.Errorf("PMT program_info_length %d invalid", pinfo)
	}
	es := tab[body:end]
	found := 0
	for i := 0; i+5 <= len(es); i += 5 + int(es[i+3]&0x0F)<<8 | 0 {
		st := es[i]
		pid := uint16(es[i+1]&0x1F)<<8 | uint16(es[i+2])
		esInfo := int(es[i+3]&0x0F)<<8 | int(es[i+4])
		if i+5+esInfo > len(es) {
			return fmt.Errorf("PMT ES_info_length overruns descriptors")
		}
		t, ok := streamType(st, pid)
		if !ok {
			continue
		}
		out[pid] = t
		found++
		i += esInfo
	}
	if found == 0 {
		return fmt.Errorf("PMT lists no supported stream")
	}
	return nil
}

func streamType(st uint8, pid uint16) (Track, bool) {
	switch st {
	case 0x1B:
		return Track{Kind: media.KindVideo, Codec: "avc1", Timescale: 90000, TrackID: uint32(pid)}, true
	case 0x24:
		return Track{Kind: media.KindVideo, Codec: "hvc1", Timescale: 90000, TrackID: uint32(pid)}, true
	case 0x0F:
		return Track{Kind: media.KindAudio, Codec: "mp4a", Timescale: 90000, SampleRate: 48000, Channels: 2, TrackID: uint32(pid)}, true
	case 0x03, 0x04:
		return Track{Kind: media.KindAudio, Codec: "mp4a", Timescale: 90000, SampleRate: 48000, Channels: 2, TrackID: uint32(pid)}, true
	case 0x81:
		return Track{Kind: media.KindAudio, Codec: "ac-3", Timescale: 90000, SampleRate: 48000, Channels: 6, TrackID: uint32(pid)}, true
	}
	return Track{}, false
}
