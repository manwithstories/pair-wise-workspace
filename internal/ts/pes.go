package ts

import (
	"encoding/binary"
	"fmt"

	"mediqc/internal/media"
)

func (d *Demuxer) feedPES(pid uint16, pusi bool, payload []byte, offset int64) *media.Error {
	if pusi {
		if old := d.pes[pid]; old != nil && len(old.buf) > 0 && old.want == 0 {
			if err := d.emitPES(pid, old); err != nil {
				return err
			}
		}
		b, perr := d.parsePESHeader(pid, payload, offset)
		if perr != nil {
			d.add(perr)
			return nil
		}
		d.pes[pid] = b
		if len(payload) > b.headerLen {
			b.buf = append(b.buf, payload[b.headerLen:]...)
		}
	} else {
		b := d.pes[pid]
		if b == nil {
			// Streams may be joined after their first PUSI; bounded ignore.
			return nil
		}
		b.buf = append(b.buf, payload...)
	}

	b := d.pes[pid]
	if b == nil {
		return nil
	}
	if d.Limits.MaxPESBytes > 0 && len(b.buf) > d.Limits.MaxPESBytes {
		err := (&media.Error{
			Code: media.CodeResourceLimit, Component: "ts.demux", Offset: offset,
			Message: fmt.Sprintf("PES buffer for PID %d exceeds limit", pid),
		}).MarkFatal()
		d.add(err)
		delete(d.pes, pid)
		return err
	}
	if b.want > 0 && len(b.buf) >= b.want {
		if err := d.emitPES(pid, b); err != nil {
			return err
		}
		delete(d.pes, pid)
	}
	return nil
}

func (d *Demuxer) parsePESHeader(pid uint16, payload []byte, offset int64) (*pesBuilder, *media.Error) {
	if len(payload) < 9 || payload[0] != 0 || payload[1] != 0 || payload[2] != 1 {
		return nil, &media.Error{Code: media.CodeLengthInvalid, Component: "ts.demux", Offset: offset, Message: "invalid PES start code"}
	}
	streamID := payload[3]
	if streamID < 0xbc {
		return nil, &media.Error{Code: media.CodeUnsupported, Component: "ts.demux", Offset: offset, Message: fmt.Sprintf("unsupported PES stream id 0x%02x", streamID)}
	}
	pesLength := int(binary.BigEndian.Uint16(payload[4:6]))
	headerDataLength := int(payload[8])
	headerEnd := 9 + headerDataLength
	if headerEnd > len(payload) || (pesLength > 0 && pesLength < 3+headerDataLength) {
		return nil, &media.Error{Code: media.CodeLengthInvalid, Component: "ts.demux", Offset: offset, Message: "PES header length invalid"}
	}
	b := &pesBuilder{pesLength: pesLength, headerLen: headerEnd}
	if pesLength > 0 {
		b.want = pesLength - (3 + headerDataLength)
	}
	ptsFlags := (payload[7] >> 6) & 0x03
	if ptsFlags == 0x02 || ptsFlags == 0x03 {
		if headerEnd < 14 {
			return nil, &media.Error{Code: media.CodeLengthInvalid, Component: "ts.demux", Offset: offset, Message: "truncated PTS"}
		}
		rawPTS := parseTimestamp(payload[9:14])
		b.pts = d.extendTimestamp(d.ptsWrap, pid, rawPTS)
		b.havePTS = true
	}
	if ptsFlags == 0x03 {
		if headerEnd < 19 {
			return nil, &media.Error{Code: media.CodeLengthInvalid, Component: "ts.demux", Offset: offset, Message: "truncated DTS"}
		}
		rawDTS := parseTimestamp(payload[14:19])
		b.dts = d.extendTimestamp(d.dtsWrap, pid, rawDTS)
		b.haveDTS = true
	}
	if !b.haveDTS && b.havePTS {
		b.dts, b.haveDTS = b.pts, true
	}
	return b, nil
}

func parseTimestamp(b []byte) uint64 {
	_ = b[4]
	return uint64((b[0]&0x0e)>>1)<<30 |
		uint64(b[1])<<22 |
		uint64((b[2]&0xfe)>>1)<<15 |
		uint64(b[3])<<7 |
		uint64((b[4]&0xfe)>>1)
}

func (d *Demuxer) extendTimestamp(m map[uint16]*media.WrapExtender, pid uint16, raw uint64) int64 {
	w := m[pid]
	if w == nil {
		w = media.NewPTSWrapExtender()
		m[pid] = w
	}
	return w.Extend(raw)
}

func (d *Demuxer) emitPES(pid uint16, b *pesBuilder) *media.Error {
	if b == nil || len(b.buf) == 0 {
		return nil
	}
	if d.Limits.MaxSamples > 0 && len(d.samples) >= d.Limits.MaxSamples {
		err := (&media.Error{
			Code: media.CodeResourceLimit, Component: "ts.demux",
			Message: fmt.Sprintf("sample count exceeds limit %d", d.Limits.MaxSamples),
		}).MarkFatal()
		d.add(err)
		return err
	}
	size := len(b.buf)
	if b.want > 0 && size > b.want {
		size = b.want
	}
	data := append([]byte(nil), b.buf[:size]...)
	idx := len(d.samples)
	s := media.Sample{
		TrackID:      0,
		PID:          pid,
		Index:        idx,
		DTS:          b.dts,
		PTS:          b.pts,
		Data:         data,
		RandomAccess: len(data) >= 4 && data[0] == 0 && data[1] == 0 && data[2] == 1,
	}
	if !b.havePTS {
		s.PTS = s.DTS
	}
	d.samples = append(d.samples, s)
	return nil
}

// FlushPES emits unbounded PES data and rejects incomplete finite PES.
func (d *Demuxer) FlushPES() *media.Error {
	for pid, b := range d.pes {
		if b == nil || len(b.buf) == 0 {
			continue
		}
		if b.want > 0 && len(b.buf) < b.want {
			err := (&media.Error{
				Code: media.CodeUnexpectedEOF, Component: "ts.demux",
				Message: fmt.Sprintf("PID %d PES has %d/%d payload bytes", pid, len(b.buf), b.want),
			}).MarkFatal()
			d.add(err)
			return err
		}
		if err := d.emitPES(pid, b); err != nil {
			return err
		}
		delete(d.pes, pid)
	}
	return nil
}
