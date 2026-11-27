package pipeline

import (
	"encoding/binary"

	"tsqc/internal/ts"
)

type muxState struct {
	cc      map[uint16]uint8
	out     []byte
	lastPCR int64
	pcrSeen bool
	videoN  int
	endPTS  int64
}

func (s *Segmenter) mux(items []segItem, seg *Segment) ([]byte, int, int, error) {
	st := &muxState{cc: map[uint16]uint8{}, lastPCR: -1, endPTS: seg.StartPTS}
	st.writePSI(s.VideoPID, s.AudioPID)
	var v, a int
	for _, it := range items {
		au := it.au
		pts, dts := au.PTS, au.DTS
		if pts < 0 {
			pts = it.pts
		}
		hasDTS := au.HasDTS
		if !hasDTS || dts < 0 {
			dts = pts
			hasDTS = false
		}
		var pcr int64 = -1
		if s.PCRPID == s.VideoPID && au.PID == s.VideoPID && st.videoN%s.PCRInterval == 0 {
			pcr = dts
			if pcr < st.lastPCR {
				pcr = st.lastPCR
			}
			st.lastPCR = pcr
			st.pcrSeen = true
		}
		if au.PID == s.VideoPID {
			st.videoN++
			v++
		} else {
			a++
		}
		pes := encodePES(au.Kind, au.PID, pts, dts, hasDTS, au.Data)
		st.writePES(au.PID, pes, pcr)
		if it.pts > st.endPTS {
			st.endPTS = it.pts
		}
	}
	seg.EndPTS = st.endPTS
	return st.out, v, a, nil
}

func (st *muxState) packet(pid uint16, pusi bool, payload []byte, pcr int64) {
	p := make([]byte, ts.PacketSize)
	p[0] = ts.SyncByte
	p[1] = byte(pid>>8) & 0x1f
	if pusi {
		p[1] |= 0x40
	}
	p[2] = byte(pid)
	cc := st.cc[pid]
	off := 4
	if pcr >= 0 {
		p[3] = 0x30 | cc&0x0f
		base := uint64(pcr / 300)
		ext := uint16(pcr % 300)
		p[4] = 7
		p[5] = 0x10
		p[6] = byte(base >> 25)
		p[7] = byte(base >> 17)
		p[8] = byte(base >> 9)
		p[9] = byte(base >> 1)
		p[10] = byte(base<<7) | byte(ext>>8)&0x01
		p[11] = byte(ext)
		off = 12
	} else {
		p[3] = 0x10 | cc&0x0f
	}
	st.cc[pid] = (cc + 1) & 0x0f
	n := copy(p[off:], payload)
	for i := off + n; i < ts.PacketSize; i++ {
		p[i] = 0xFF
	}
	st.out = append(st.out, p...)
}

func (st *muxState) writePSI(videoPID, audioPID uint16) {
	patBody := []byte{}
	if s := makeSection(0x00, ProgramNumber, 0, func(body *[]byte) {
		pmt := uint16(PMTPID)
		*body = append(*body, byte(ProgramNumber>>8), byte(ProgramNumber), byte(pmt>>8)|0xE0, byte(pmt))
	}); s != nil {
		patBody = s
	}
	st.packet(ts.PIDPAT, true, append([]byte{0}, patBody...), -1)

	pmtBody := makeSection(0x02, ProgramNumber, 0, func(body *[]byte) {
		*body = append(*body, byte(videoPID>>8)|0xE0, byte(videoPID), 0xF0, 0x00)
		*body = append(*body, ts.StreamH264, 0xE0|byte(videoPID>>8), byte(videoPID), 0xF0, 0x00)
		*body = append(*body, ts.StreamAAC, 0xE0|byte(audioPID>>8), byte(audioPID), 0xF0, 0x00)
	})
	st.packet(PMTPID, true, append([]byte{0}, pmtBody...), -1)
}

func (st *muxState) writePES(pid uint16, pes []byte, pcr int64) {
	first := true
	for len(pes) > 0 {
		limit := 184
		if first && pcr >= 0 {
			limit = 176
		}
		n := len(pes)
		if n > limit {
			n = limit
		}
		p := pcr
		if !first {
			p = -1
		}
		st.packet(pid, first, pes[:n], p)
		pes = pes[n:]
		first = false
		pcr = -1
	}
}

func makeSection(tableID uint8, ext uint16, version uint8, fill func(*[]byte)) []byte {
	var body []byte
	fill(&body)
	length := len(body) + 5 + 4
	sec := make([]byte, 3+length)
	sec[0] = tableID
	sec[1] = 0xB0 | byte(length>>8&0x0f)
	sec[2] = byte(length)
	binary.BigEndian.PutUint16(sec[3:], ext)
	sec[5] = 0xC1 | version<<1&0x1e
	sec[6], sec[7] = 0, 0
	copy(sec[8:], body)
	binary.BigEndian.PutUint32(sec[len(sec)-4:], ts.CRC32(sec[:len(sec)-4]))
	return sec
}

func encodePES(kind ts.Kind, pid uint16, pts, dts int64, hasDTS bool, data []byte) []byte {
	streamID := byte(0xE0)
	if kind == ts.KindAudio || pid == 0 {
		streamID = 0xC0
	}
	flags := byte(0x80)
	hlen := 5
	if hasDTS {
		flags |= 0x40
		hlen += 5
	}
	start := 9 + hlen
	total := start + len(data)
	pktLen := 0
	// Bounded lengths let the demux recover the exact payload boundary
	// instead of relying on stuffing-byte heuristics for unbounded video PES.
	if total-6 <= 0xffff {
		pktLen = total - 6
	}
	h := make([]byte, start)
	h[0], h[1], h[2], h[3] = 0, 0, 1, streamID
	binary.BigEndian.PutUint16(h[4:], uint16(pktLen))
	h[6] = 0x80
	h[7] = flags
	h[8] = byte(hlen)
	// PES timestamps are 90 kHz values; the demux multiplies them back to
	// the 27 MHz shared time base.
	putStamp(h[9:], pts/300)
	if hasDTS {
		putStamp(h[14:], dts/300)
	}
	return append(h, data...)
}

func putStamp(b []byte, v int64) {
	u := uint64(v)
	b[0] = 0x21 | byte(u>>29)&0x0e
	b[1] = byte(u >> 22)
	b[2] = 0x01 | byte(u>>14)&0xfe
	b[3] = byte(u >> 7)
	b[4] = 0x01 | byte(u<<1)&0xfe
}
