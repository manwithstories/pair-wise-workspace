package sample

import (
	"encoding/binary"
	"math/rand"
)

// DamageConfig controls the deterministic network-level perturbation applied
// to a rendition's datagram stream.
type DamageConfig struct {
	Loss        float64 // fraction of eligible (non-IDR) RTP packets dropped
	ReorderDist int     // maximum swap distance for out-of-order delivery
	Duplicates  float64 // fraction of packets duplicated
}

// Stats reports what the generator did to a clean stream.
type Stats struct {
	RTPDatagrams int
	SRReports    int
	Lost         int
	Reordered    int
	Duplicated   int
}

// dgram is one wire datagram plus the flag telling whether it protects an IDR
// frame (RTCP datagrams always have isIDR=false).
type dgram struct {
	raw   []byte
	isIDR bool
}

// rtpPacket builds an RFC 2250 RTP datagram (MPEG-TS payload, 90 kHz clock).
func rtpPacket(ssrc uint32, seq uint16, ts uint32, marker bool, payload []byte) []byte {
	h := make([]byte, 12)
	h[0] = 0x80 // V2, no markers/CSRC
	h[1] = 33   // dynamic payload type for MP2T
	if marker {
		h[1] |= 0x80
	}
	binary.BigEndian.PutUint16(h[2:4], seq)
	binary.BigEndian.PutUint32(h[4:8], ts)
	binary.BigEndian.PutUint32(h[8:12], ssrc)
	return append(h, payload...)
}

// srPacket builds a 28-byte RTCP sender report datagram.
func srPacket(ssrc uint32, ntp uint64, rtpTS, packetCount uint32) []byte {
	b := make([]byte, 28)
	b[0], b[1] = 0x80, 200
	binary.BigEndian.PutUint16(b[2:4], 6)
	binary.BigEndian.PutUint32(b[4:8], ssrc)
	binary.BigEndian.PutUint64(b[8:16], ntp)
	binary.BigEndian.PutUint32(b[16:20], rtpTS)
	binary.BigEndian.PutUint32(b[20:24], packetCount)
	return b
}

// Frame prefixes a datagram with the two-byte big-endian length used by the
// archive wire format.
func lengthPrefix(b []byte) []byte {
	out := make([]byte, 2+len(b))
	binary.BigEndian.PutUint16(out, uint16(len(b)))
	copy(out[2:], b)
	return out
}

// tsPerRTP is the number of MPEG-TS packets packed into one RTP datagram. Seven
// 188-byte packets (1316 bytes) is the de-facto RFC 2250 default.
const tsPerRTP = 7

// BuildRendition synthesizes one rendition's full framed datagram stream for
// nFrames video frames, then applies the deterministic damage described by
// cfg. IDR-bearing RTP packets are exempt from loss so keyframe alignment is
// always recoverable; loss lands on non-IDR packets only.
func BuildRendition(r Rendition, nFrames int, seed int64, cfg DamageConfig) ([]byte, Stats, error) {
	rng := rand.New(rand.NewSource(seed))
	cc := &ccState{}

	// 1) Build the clean TS packet sequence, tagged with frame metadata so we
	// know which RTP packets protect an IDR.
	type tsUnit struct {
		packet []byte
		frame  int
		isIDR  bool
		marker bool
		rtpTS  uint32
		pad    bool // alignment stuffing, never carries video
	}
	var units []tsUnit
	psiCounter := 0
	emitPSI := func(fi int) {
		for _, p := range psPacket(cc) {
			units = append(units, tsUnit{packet: p, frame: fi})
		}
		// Pad to an RTP packetization boundary so the following video frame
		// starts at the first TS slot of a fresh RTP datagram. This keeps every
		// IDR frame's leading packet inside an IDR-flagged RTP datagram.
		for len(units)%tsPerRTP != 0 {
			stuff := make([]byte, TSPacketSize)
			writeHeader(stuff, PIDVideo, false, true, 0x0F)
			for k := 4; k < TSPacketSize; k++ {
				stuff[k] = 0xFF
			}
			units = append(units, tsUnit{packet: stuff, frame: fi, pad: true})
		}
	}
	for fi := 0; fi < nFrames; fi++ {
		if fi%SegmentFrames == 0 || psiCounter <= 0 {
			emitPSI(fi)
			psiCounter = StreamPSIInterval
		}
		fr := buildFrame(fi, cc, r.TSPerFrame)
		for k, p := range fr.TSPackets {
			units = append(units, tsUnit{
				packet: p, frame: fi, isIDR: fr.IsIDR,
				marker: k == len(fr.TSPackets)-1, rtpTS: fr.RTPTime,
			})
		}
		psiCounter -= r.TSPerFrame
	}

	// 2) Packetize into RTP datagrams of tsPerRTP TS packets.
	var dgrams []dgram
	var seq uint16
	pktCount := uint32(0)
	for i := 0; i < len(units); i += tsPerRTP {
		end := i + tsPerRTP
		if end > len(units) {
			end = len(units)
		}
		payload := make([]byte, 0, (end-i)*TSPacketSize)
		isIDR, marker, rtpTS := false, false, units[i].rtpTS
		for _, u := range units[i:end] {
			payload = append(payload, u.packet...)
			isIDR = isIDR || u.isIDR
			marker = marker || u.marker
			if u.rtpTS != 0 {
				rtpTS = u.rtpTS
			}
		}
		dgrams = append(dgrams, dgram{raw: rtpPacket(r.SSRC, seq, rtpTS, marker, payload), isIDR: isIDR})
		seq++
		pktCount++
	}

	st := Stats{RTPDatagrams: len(dgrams)}

	// 3) Insert RTCP sender reports at fixed media intervals (every ~5 s).
	// Reports are emitted as standalone framed datagrams interspersed in order.
	var withSR []dgram
	srEvery := 5 * FramesPerSecond * r.TSPerFrame / (tsPerRTP * TSPacketSize / TSPacketSize)
	if srEvery == 0 {
		srEvery = 18
	}
	for i, dg := range dgrams {
		if i%srEvery == 0 {
			ntp := ntpFromRTP(dgrams[i].raw)
			sr := srPacket(r.SSRC, ntp, binary.BigEndian.Uint32(dgrams[i].raw[4:8]), pktCount)
			withSR = append(withSR, dgram{raw: sr})
			st.SRReports++
		}
		withSR = append(withSR, dg)
	}

	// 4) Apply deterministic damage to RTP datagrams only (never to RTCP).
	out, dmg := perturb(rng, withSR, cfg)
	st.Lost, st.Reordered, st.Duplicated = dmg.lost, dmg.reordered, dmg.duplicated
	return out, st, nil
}

// ntpFromRTP derives a plausible NTP anchor (epoch base 1<<32) from an RTP
// packet's timestamp for fixture purposes.
func ntpFromRTP(rtp []byte) uint64 {
	ts := binary.BigEndian.Uint32(rtp[4:8])
	return (1 << 32) + (uint64(ts)<<32)/ClockRate
}

type damageCounters struct{ lost, reordered, duplicated int }

// perturb applies loss (non-IDR only), bounded reordering and duplication.
// RTCP datagrams are detected by payload type and passed through untouched.
func perturb(rng *rand.Rand, in []dgram, cfg DamageConfig) ([]byte, damageCounters) {
	var dc damageCounters
	// Work on indices so reordering can move whole datagrams.
	type idx struct {
		pos  int
		rtcp bool
	}
	order := make([]idx, 0, len(in))
	dropped := make(map[int]bool)
	dup := make(map[int]bool)
	for i, dg := range in {
		isRTCP := len(dg.raw) >= 2 && dg.raw[1]&0x7F >= 64 && dg.raw[1]&0x7F <= 95
		if !isRTCP && !dg.isIDR && cfg.Loss > 0 && rng.Float64() < cfg.Loss {
			dropped[i] = true
			dc.lost++
			continue
		}
		if !isRTCP && cfg.Duplicates > 0 && rng.Float64() < cfg.Duplicates {
			dup[i] = true
			dc.duplicated++
		}
		order = append(order, idx{pos: i, rtcp: isRTCP})
	}
	// Bounded swaps between adjacent-ish RTP datagrams (never across RTCP).
	if cfg.ReorderDist > 0 {
		for k := 0; k+1 < len(order); k++ {
			if order[k].rtcp || order[k+1].rtcp {
				continue
			}
			if rng.Intn(cfg.ReorderDist) == 0 {
				order[k], order[k+1] = order[k+1], order[k]
				dc.reordered++
				k++ // skip the swapped partner
			}
		}
	}
	var out []byte
	emit := func(i int) {
		out = append(out, lengthPrefix(in[i].raw)...)
	}
	for _, e := range order {
		emit(e.pos)
		if dup[e.pos] {
			emit(e.pos)
		}
	}
	return out, dc
}
