package tsslice

import (
	"bufio"
	"fmt"
	"io"
)

// Kind classifies the elementary stream a packet belongs to.
type Kind int

const (
	KindOther Kind = iota
	KindVideo
	KindAudio
)

// Info describes one scanned TS packet.
type Info struct {
	Index    int64 // packet index within the scanned stream
	Kind     Kind
	Keyframe bool // random-access indicator on a video packet
	HasPTS   bool
	PTS      int64 // 90kHz clock
	HasPCR   bool
	PCR      int64 // 90kHz base
}

// Scan reads TS packets from r and invokes h for each packet in order.
// A truncated tail packet is tolerated to model weak-network captures.
func Scan(r io.Reader, h func(Info) error) error {
	br := bufio.NewReaderSize(r, 1<<20)
	pmtPID, videoPID, audioPID := -1, -1, -1
	buf := make([]byte, PacketSize)
	var index int64
	for {
		if _, err := io.ReadFull(br, buf); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		if buf[0] != SyncByte {
			return fmt.Errorf("tsslice: lost sync at packet %d", index)
		}
		pid := uint16(buf[1]&0x1F)<<8 | uint16(buf[2])
		pusi := buf[1]&0x40 != 0
		afc := (buf[3] >> 4) & 0x03
		off := 4
		info := Info{Index: index}
		if afc == 2 || afc == 3 {
			l := int(buf[4])
			if l >= 1 && 5+l <= PacketSize {
				flags := buf[5]
				if flags&0x40 != 0 {
					info.Keyframe = true
				}
				if flags&0x10 != 0 && l >= 7 {
					info.HasPCR = true
					info.PCR = decodePCR(buf[6:12])
				}
			}
			off = 5 + l
		}
		if off > PacketSize {
			return fmt.Errorf("tsslice: bad adaptation field at packet %d", index)
		}
		payload := buf[off:]
		switch {
		case pid == PIDPAT && pusi:
			pmtPID = parsePAT(payload)
		case pmtPID >= 0 && pid == uint16(pmtPID) && pusi:
			videoPID, audioPID = parsePMT(payload)
		case videoPID >= 0 && pid == uint16(videoPID):
			info.Kind = KindVideo
			if pusi {
				if pts, ok := parsePESPTS(payload); ok {
					info.HasPTS, info.PTS = true, pts
				}
			}
		case audioPID >= 0 && pid == uint16(audioPID):
			info.Kind = KindAudio
			if pusi {
				if pts, ok := parsePESPTS(payload); ok {
					info.HasPTS, info.PTS = true, pts
				}
			}
		}
		if info.Kind != KindVideo {
			info.Keyframe = false
		}
		if err := h(info); err != nil {
			return err
		}
		index++
	}
}

func parsePAT(payload []byte) int {
	if len(payload) < 12 {
		return -1
	}
	t := payload[1+int(payload[0]):]
	if len(t) < 12 || t[0] != 0x00 {
		return -1
	}
	sectionLen := int(t[1]&0x0F)<<8 | int(t[2])
	end := 3 + sectionLen - 4
	for i := 8; i+4 <= end && i+4 <= len(t); i += 4 {
		program := int(t[i])<<8 | int(t[i+1])
		if program != 0 {
			return int(t[i+2]&0x1F)<<8 | int(t[i+3])
		}
	}
	return -1
}

func parsePMT(payload []byte) (video, audio int) {
	video, audio = -1, -1
	if len(payload) < 16 {
		return
	}
	t := payload[1+int(payload[0]):]
	if len(t) < 16 || t[0] != 0x02 {
		return
	}
	sectionLen := int(t[1]&0x0F)<<8 | int(t[2])
	progInfo := int(t[10]&0x0F)<<8 | int(t[11])
	end := 3 + sectionLen - 4
	for i := 12 + progInfo; i+5 <= end && i+5 <= len(t); {
		streamType := t[i]
		elemPID := int(t[i+1]&0x1F)<<8 | int(t[i+2])
		esInfo := int(t[i+3]&0x0F)<<8 | int(t[i+4])
		switch streamType {
		case 0x1B: // H.264
			video = elemPID
		case 0x0F: // AAC ADTS
			audio = elemPID
		}
		i += 5 + esInfo
	}
	return
}

func parsePESPTS(payload []byte) (int64, bool) {
	if len(payload) < 14 || payload[0] != 0x00 || payload[1] != 0x00 || payload[2] != 0x01 {
		return 0, false
	}
	if payload[7]&0x80 == 0 {
		return 0, false
	}
	return decodePTS(payload[9:14]), true
}

func decodePTS(b []byte) int64 {
	return int64(b[0]>>1&0x07)<<30 |
		int64(b[1])<<22 |
		int64(b[2]>>1)<<15 |
		int64(b[3])<<7 |
		int64(b[4]>>1)
}

func decodePCR(b []byte) int64 {
	return int64(b[0])<<25 |
		int64(b[1])<<17 |
		int64(b[2])<<9 |
		int64(b[3])<<1 |
		int64(b[4])>>7
}
