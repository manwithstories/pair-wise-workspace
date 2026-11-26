package segment

import (
	"fmt"

	"aircheck/internal/media"
)

// TSOptions tune TS demuxing.
type TSOptions struct {
	// DefaultAudioSamples is used for an audio access unit whose duration
	// cannot be derived from a neighbouring timestamp (e.g. a segment with a
	// single audio PES). 1024 matches one AAC-LC frame.
	DefaultAudioSamples int
}

// TS constants.
const (
	tsPacketSize = 188
	tsSyncByte   = 0x47
	tsWrap       = int64(1) << 33

	pidPAT  = 0x0000
	pidNull = 0x1FFF
)

type tsPacket struct {
	offset     int64
	pid        uint16
	pusi       bool
	cc         uint8
	hasPayload bool
	discont    bool
	payload    []byte
}

// DemuxTS parses an MPEG-TS segment. It validates 188-byte framing, the
// transport sync byte and each PID's continuity counter, reassembles PES
// packets, and normalises 33-bit timestamp wraparound. Any short read is
// reported as FaultTruncated at the original byte offset.
func DemuxTS(artifact string, data []byte, opt TSOptions) (*Result, error) {
	if opt.DefaultAudioSamples == 0 {
		opt.DefaultAudioSamples = 1024
	}
	res := &Result{Format: FormatTS, Artifact: artifact, SizeBytes: int64(len(data)), Offset0: 0}
	if len(data) == 0 {
		return nil, media.NewFault(media.FaultTruncated, media.SourcePos{Artifact: artifact, Offset: 0}, "empty TS segment")
	}
	full := len(data) / tsPacketSize
	if rem := len(data) % tsPacketSize; rem != 0 {
		return nil, media.NewFault(media.FaultTruncated,
			media.SourcePos{Artifact: artifact, Offset: int64(full * tsPacketSize)},
			fmt.Sprintf("trailing %d bytes are not a complete 188-byte packet", rem))
	}

	var pmtPID uint16
	pmtSeen := false
	pmtValid := false
	streams := map[uint16]Track{}

	var cur *pesBuffer
	var pesList []*pesBuffer
	flush := func() {
		if cur != nil {
			pesList = append(pesList, cur)
			cur = nil
		}
	}

	lastCC := map[uint16]uint8{}
	ccSeen := map[uint16]bool{}

	for i := 0; i < full; i++ {
		off := int64(i * tsPacketSize)
		b := data[i*tsPacketSize : (i+1)*tsPacketSize]
		p, err := parseTSPacket(artifact, b, off)
		if err != nil {
			return nil, err
		}
		if p.pid == pidNull {
			continue
		}
		if ccSeen[p.pid] && p.hasPayload && !p.discont {
			if p.cc != (lastCC[p.pid]+1)&0x0F {
				return nil, media.NewFault(media.FaultContinuity,
					media.SourcePos{Artifact: artifact, Offset: off},
					fmt.Sprintf("PID 0x%04x continuity %d expected %d", p.pid, p.cc, (lastCC[p.pid]+1)&0x0F))
			}
		}
		ccSeen[p.pid] = true
		lastCC[p.pid] = p.cc

		switch {
		case p.pid == pidPAT:
			if p.pusi {
				if err := parsePAT(p.payload, &pmtPID, &pmtSeen); err != nil {
					return nil, media.WrapFault(media.FaultManifestSyntax,
						media.SourcePos{Artifact: artifact, Offset: off}, err)
				}
			}
		case pmtSeen && p.pid == pmtPID:
			if p.pusi {
				if err := parsePMT(p.payload, streams); err != nil {
					return nil, media.WrapFault(media.FaultUnsupportedCodec,
						media.SourcePos{Artifact: artifact, Offset: off}, err)
				}
				pmtValid = true
			}
		default:
			if !pmtValid {
				continue // unknown PID until the PMT is known
			}
			if _, ok := streams[p.pid]; !ok {
				continue
			}
			if p.pusi {
				flush()
				cur = &pesBuffer{pid: p.pid, offset: off, ptsStart: true}
			}
			if cur == nil {
				// Payload before a PES start: stream is missing its first
				// unit, report where it was lost.
				return nil, media.NewFault(media.FaultTruncated,
					media.SourcePos{Artifact: artifact, Offset: off},
					fmt.Sprintf("PES data on PID 0x%04x before payload_unit_start", p.pid))
			}
			cur.buf = append(cur.buf, p.payload...)
		}
	}
	flush()
	if !pmtSeen || !pmtValid {
		return nil, media.NewFault(media.FaultMissingInit, media.SourcePos{Artifact: artifact, Offset: 0},
			"no PAT/PMT carried in the TS segment")
	}
	if len(streams) == 0 {
		return nil, media.NewFault(media.FaultUnsupportedCodec, media.SourcePos{Artifact: artifact},
			"PMT lists no video or audio stream")
	}

	trackOf := func(pid uint16) Track { return streams[pid] }
	if err := assemblePackets(res, pesList, trackOf, opt); err != nil {
		return nil, err
	}
	if len(res.Packets) == 0 {
		return nil, media.NewFault(media.FaultTruncated, media.SourcePos{Artifact: artifact},
			"no elementary payload recovered")
	}

	res.Tracks = []Track{}
	seen := map[uint32]bool{}
	for _, p := range res.Packets {
		if seen[p.TrackID] {
			continue
		}
		seen[p.TrackID] = true
		if t, ok := trackByID(streams, p.TrackID); ok {
			res.Tracks = append(res.Tracks, t)
		}
	}
	res.Sort()
	return res, nil
}

type pesBuffer struct {
	pid      uint16
	offset   int64
	ptsStart bool
	buf      []byte
}

func assemblePackets(res *Result, list []*pesBuffer, trackOf func(uint16) Track, opt TSOptions) error {
	type pending struct {
		pid   uint16
		start int64
		dts   int64
		pts   int64
		data  []byte
		key   bool
		tr    Track
	}
	var pend []pending
	var wraps int64
	var lastDTS = map[uint16]int64{}
	var haveLast = map[uint16]bool{}

	for _, pb := range list {
		if len(pb.buf) < 9 {
			return media.NewFault(media.FaultTruncated,
				media.SourcePos{Artifact: res.Artifact, Offset: pb.offset}, "PES header shorter than 9 bytes")
		}
		if pb.buf[0] != 0 || pb.buf[1] != 0 || pb.buf[2] != 0x01 {
			return media.NewFault(media.FaultTruncated,
				media.SourcePos{Artifact: res.Artifact, Offset: pb.offset}, "PES missing start code")
		}
		streamID := pb.buf[3]
		hlen := int(pb.buf[8])
		if 9+hlen > len(pb.buf) {
			return media.NewFault(media.FaultTruncated,
				media.SourcePos{Artifact: res.Artifact, Offset: pb.offset}, "PES header_data_length overruns packet")
		}
		var pts, dts int64
		havePTS := false
		flags := pb.buf[7]
		if flags&0x80 != 0 && hlen >= 5 {
			pts = readTimestamp(pb.buf[9 : 9+5])
			havePTS = true
		}
		if flags&0xC0 == 0xC0 && hlen >= 10 {
			dts = readTimestamp(pb.buf[14 : 14+5])
		} else if havePTS {
			dts = pts
		}
		payload := pb.buf[9+hlen:]
		if !havePTS {
			return media.NewFault(media.FaultTruncated,
				media.SourcePos{Artifact: res.Artifact, Offset: pb.offset},
				fmt.Sprintf("PES stream 0x%02x has no PTS", streamID))
		}
		// Normalise 33-bit wraparound per PID.
		if haveLast[pb.pid] {
			if dts < lastDTS[pb.pid] && lastDTS[pb.pid]-dts > tsWrap/2 {
				wraps++
			}
		}
		haveLast[pb.pid] = true
		lastDTS[pb.pid] = dts
		dts += wraps * tsWrap
		if havePTS {
			pts += wraps * tsWrap
		}
		p := pending{pid: pb.pid, start: pb.offset, dts: dts, pts: pts, data: payload, tr: trackOf(pb.pid)}
		if p.tr.Kind == media.KindVideo {
			p.key = h264IDR(payload)
		}
		pend = append(pend, p)
	}
	if wraps > 0 {
		res.Wraps = int(wraps)
	}

	// Derive per-track durations and audio sample counts from PTS deltas.
	for i := range pend {
		if pend[i].pid == 0 {
			continue
		}
		tr := pend[i].tr
		delta := int64(0)
		for j := i + 1; j < len(pend); j++ {
			if pend[j].pid == pend[i].pid {
				delta = pend[j].dts - pend[i].dts
				break
			}
		}
		if delta <= 0 {
			for j := i - 1; j >= 0; j-- {
				if pend[j].pid == pend[i].pid {
					delta = pend[i].dts - pend[j].dts
					break
				}
			}
		}
		samples := 0
		if tr.Kind == media.KindAudio {
			if delta > 0 && tr.SampleRate > 0 {
				samples = int(delta * int64(tr.SampleRate) / 90000)
			}
			if samples <= 0 {
				samples = opt.DefaultAudioSamples
			}
		}
		res.Packets = append(res.Packets, Packet{
			Kind: tr.Kind, TrackID: tr.TrackID,
			DTS: pend[i].dts, PTS: pend[i].pts, Duration: delta,
			Samples: samples, Keyframe: pend[i].key, Data: pend[i].data,
			SourceOffset: pend[i].start,
		})
	}
	return nil
}

// readTimestamp decodes the 33-bit PTS/DTS field.
func readTimestamp(b []byte) int64 {
	if len(b) < 5 {
		return 0
	}
	return int64(b[0]&0x0E)<<29 |
		int64(b[1])<<22 |
		int64(b[2]&0xFE)<<14 |
		int64(b[3])<<7 |
		int64(b[4])>>1
}

// h264IDR reports whether an Annex-B payload contains an IDR slice (type 5).
func h264IDR(b []byte) bool {
	for i := 0; i+3 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			t := b[i+3] & 0x1F
			if t == 5 {
				return true
			}
		}
	}
	return false
}
