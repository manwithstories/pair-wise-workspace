package demux

import (
	"bytes"
	"fmt"

	"fieldpost/internal/core"
)

// TSPacketSize is the fixed MPEG-TS packet size used by FieldPost fixtures.
const TSPacketSize = 188

const (
	tsPIDVideo = 0x0100
	tsPIDAudio = 0x0101
)

type pesAssembly struct {
	pid  uint16
	buf  bytes.Buffer
	cc   int
	seen bool
}

// ParseTS streams through fixed 188-byte MPEG-TS packets and reassembles PES
// access units. It never pads a truncated packet or fabricates missing time.
func ParseTS(segmentID, source string, data []byte) (*core.Segment, error) {
	if len(data) == 0 {
		return nil, core.UnrecoverableError(core.StageDemux, source, segmentID, 0, "empty TS segment")
	}
	if rem := len(data) % TSPacketSize; rem != 0 {
		off := int64(len(data) - rem)
		return nil, core.UnrecoverableError(core.StageDemux, source, segmentID, off,
			"truncated TS packet: %d trailing bytes", rem)
	}
	asms := map[uint16]*pesAssembly{}
	var samples []core.Sample
	packetCount := len(data) / TSPacketSize

	for i := 0; i < packetCount; i++ {
		base := i * TSPacketSize
		pkt := data[base : base+TSPacketSize]
		if pkt[0] != 0x47 {
			return nil, core.UnrecoverableError(core.StageDemux, source, segmentID, int64(base),
				"invalid TS sync byte 0x%02x", pkt[0])
		}
		pusi := pkt[1]&0x40 != 0
		pid := uint16(pkt[1]&0x1f)<<8 | uint16(pkt[2])
		if pid == 0x1fff {
			continue
		}
		ac := (pkt[3] >> 4) & 0x03
		cc := int(pkt[3] & 0x0f)
		a := asms[pid]
		if a == nil {
			a = &pesAssembly{pid: pid, cc: cc}
			asms[pid] = a
		} else if cc != (a.cc+1)%16 {
			return nil, core.UnrecoverableError(core.StageDemux, source, segmentID, int64(base),
				"TS continuity gap on pid %d: got %d want %d", pid, cc, (a.cc+1)%16)
		}
		a.cc = cc

		payloadStart := 4
		switch ac {
		case 0:
			return nil, core.UnrecoverableError(core.StageDemux, source, segmentID, int64(base+3),
				"reserved TS adaptation control 0")
		case 2, 3:
			if len(pkt) <= 4 {
				return nil, core.UnrecoverableError(core.StageDemux, source, segmentID, int64(base+4),
					"missing TS adaptation field length")
			}
			al := int(pkt[4])
			payloadStart = 5 + al
			if payloadStart > TSPacketSize {
				return nil, core.UnrecoverableError(core.StageDemux, source, segmentID, int64(base+4),
					"TS adaptation field length %d overruns packet", al)
			}
		}
		if ac == 2 {
			continue // adaptation only
		}
		payload := pkt[payloadStart:]

		if pusi {
			if a.buf.Len() > 0 {
				sm, err := parsePES(segmentID, source, int64(base), a.pid, a.buf.Bytes())
				if err != nil {
					return nil, err
				}
				samples = append(samples, sm)
			}
			a.buf.Reset()
		}
		if _, err := a.buf.Write(payload); err != nil {
			return nil, core.Wrap(core.StageDemux, core.ExitUnrecoverable, false, err, "buffer TS payload")
		}
	}
	for pid, a := range asms {
		if a.buf.Len() == 0 {
			continue
		}
		sm, err := parsePES(segmentID, source, int64(len(data)), pid, a.buf.Bytes())
		if err != nil {
			return nil, err
		}
		samples = append(samples, sm)
	}
	if len(samples) == 0 {
		return nil, core.UnrecoverableError(core.StageDemux, source, segmentID, 0, "TS contains no PES access units")
	}
	tracks := []core.Track{
		{ID: tsPIDVideo, Kind: core.TrackVideo, Codec: "avc1", Timescale: 90000},
		{ID: tsPIDAudio, Kind: core.TrackAudio, Codec: "mp4a", Timescale: 90000, Channels: 2},
	}
	if err := finalizeSamples(segmentID, source, tracks, samples); err != nil {
		return nil, err
	}
	return &core.Segment{ID: segmentID, Source: source, Tracks: tracks, Samples: samples}, nil
}

func parsePES(segmentID, source string, offset int64, pid uint16, b []byte) (core.Sample, error) {
	if len(b) < 9 || b[0] != 0 || b[1] != 0 || b[2] != 1 {
		return core.Sample{}, core.UnrecoverableError(core.StageDemux, source, segmentID, offset,
			"invalid PES start code for pid %d", pid)
	}
	streamID := b[3]
	if pid == tsPIDVideo && streamID < 0xe0 {
		return core.Sample{}, core.UnrecoverableError(core.StageDemux, source, segmentID, offset+3,
			"video pid has non-video stream id 0x%02x", streamID)
	}
	pesLen := int(b[4])<<8 | int(b[5])
	if pesLen > 0 && 6+pesLen > len(b) {
		return core.Sample{}, core.UnrecoverableError(core.StageDemux, source, segmentID, offset+4,
			"truncated PES length %d have %d", pesLen, len(b)-6)
	}
	hdrLen := int(b[8])
	if 9+hdrLen > len(b) || hdrLen < 5 {
		return core.Sample{}, core.UnrecoverableError(core.StageDemux, source, segmentID, offset+8,
			"invalid PES header length %d", hdrLen)
	}
	flags := b[7] >> 6
	pos := 9
	var pts, dts core.Ticks
	var err error
	switch flags {
	case 0x02: // PTS only
		pts, err = decodePTS(b[pos : pos+5])
		if err != nil {
			return core.Sample{}, core.UnrecoverableError(core.StageDemux, source, segmentID, offset+9, "%v", err)
		}
		dts = pts
	case 0x03: // PTS and DTS
		pts, err = decodePTS(b[pos : pos+5])
		if err != nil {
			return core.Sample{}, core.UnrecoverableError(core.StageDemux, source, segmentID, offset+9, "%v", err)
		}
		dts, err = decodePTS(b[pos+5 : pos+10])
		if err != nil {
			return core.Sample{}, core.UnrecoverableError(core.StageDemux, source, segmentID, offset+14, "%v", err)
		}
	default:
		return core.Sample{}, core.UnrecoverableError(core.StageDemux, source, segmentID, offset+7,
			"PES missing PTS/DTS flags=%d", flags)
	}
	payload := append([]byte(nil), b[9+hdrLen:]...)
	var trackID uint32 = uint32(pid)
	sf := flagsFromEssence(trackID, payload)
	return core.Sample{TrackID: trackID, PTS: pts, DTS: dts, Flags: sf, Data: payload, DataOffset: offset}, nil
}

func decodePTS(b []byte) (core.Ticks, error) {
	if len(b) != 5 {
		return 0, fmt.Errorf("PTS field must be 5 bytes")
	}
	v := uint64(b[0]&0x0e)<<29 |
		uint64(b[1])<<22 |
		uint64(b[2]&0xfe)<<14 |
		uint64(b[3])<<7 |
		uint64(b[4]&0xfe)>>1
	return core.Ticks(v), nil
}

func flagsFromEssence(trackID uint32, payload []byte) uint32 {
	if trackID != tsPIDVideo {
		return core.FlagSync
	}
	nal := firstNALType(payload)
	if nal == 5 {
		return core.FlagSync
	}
	return core.FlagDepends
}

func firstNALType(b []byte) byte {
	for i := 0; i+4 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			return b[i+3] & 0x1f
		}
		if i+5 < len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1 {
			return b[i+4] & 0x1f
		}
	}
	if len(b) > 0 {
		return b[0] & 0x1f
	}
	return 0
}

func finalizeSamples(segmentID, source string, tracks []core.Track, samples []core.Sample) error {
	byTrack := map[uint32][]int{}
	for i := range samples {
		id := samples[i].TrackID
		if _, ok := trackByID(tracks, id); !ok {
			return core.UnrecoverableError(core.StageDemux, source, segmentID, int64(i),
				"sample references unknown track %d", id)
		}
		byTrack[id] = append(byTrack[id], i)
	}
	for id, idxs := range byTrack {
		tl := core.Timeline{}
		var prevDelta core.Ticks
		for j, idx := range idxs {
			sm := &samples[idx]
			if err := tl.Observe(sm.DTS); err != nil {
				fe, _ := core.AsError(err)
				if fe != nil {
					fe.Source = source
					fe.Segment = segmentID
					fe.Offset = int64(idx)
					return fe
				}
				return err
			}
			if j > 0 {
				prev := samples[idxs[j-1]].DTS
				d := sm.DTS - prev
				if d <= 0 {
					return core.InvariantError(core.StageDemux, source, int64(idx),
						"non-positive sample duration on track %d", id)
				}
				samples[idxs[j-1]].Duration = d
				prevDelta = d
			}
		}
		last := &samples[idxs[len(idxs)-1]]
		if last.Duration == 0 {
			if prevDelta > 0 {
				last.Duration = prevDelta
			} else if id == tsPIDVideo {
				last.Duration = 3000
			} else {
				last.Duration = 19200
			}
		}
	}
	return nil
}

func trackByID(tracks []core.Track, id uint32) (core.Track, bool) {
	for _, t := range tracks {
		if t.ID == id {
			return t, true
		}
	}
	return core.Track{}, false
}
