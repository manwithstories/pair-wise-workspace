package fixture

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"avpipe/internal/media"
	"avpipe/internal/ts"
)

const (
	Slots          = 12
	PacketsPerSlot = 80
	PacketsPerRep  = Slots * PacketsPerSlot
	VideoPID       = 0x0100
	AudioPID       = 0x0101
	PMTPID         = 0x1000
)

type Case string

const (
	CaseClean      Case = "clean"
	CaseDamaged    Case = "damaged"
	CaseTruncated  Case = "truncated"
	CasePCRWrap    Case = "pcr_wrap"
	CaseCCJump     Case = "cc_jump"
	CasePSIBad     Case = "psi_bad"
	CaseMissingIDR Case = "missing_idr"
)

type Config struct {
	Seed     int
	Case     Case
	Bitrates []int64
	Segments int
}

type Source struct {
	Rendition string
	Bitrate   int64
	Data      []byte
}

func DefaultConfig() Config {
	return Config{Seed: 23, Case: CaseClean, Bitrates: []int64{600000, 1200000}, Segments: Slots}
}

// Generate builds two deterministic 960-packet renditions. The pair therefore
// contains exactly 1920 framed TS packets before truncation.
func Generate(cfg Config) ([]Source, error) {
	if cfg.Segments <= 0 {
		cfg.Segments = Slots
	}
	if len(cfg.Bitrates) < 2 {
		return nil, fmt.Errorf("fixture requires at least two bitrates")
	}
	var out []Source
	for ri, br := range cfg.Bitrates {
		data, err := buildRendition(cfg, ri, br)
		if err != nil {
			return nil, err
		}
		if cfg.Case == CaseTruncated && ri == 0 {
			// Cut inside the final 188-byte packet, leaving a 151-byte tail.
			data = data[:len(data)-37]
		}
		name := fmt.Sprintf("r%d_%d", ri, br)
		out = append(out, Source{Rendition: name, Bitrate: br, Data: data})
	}
	return out, nil
}

func buildRendition(cfg Config, ri int, bitrate int64) ([]byte, error) {
	var stream []byte
	videoCC, audioCC, pmtCC := byte(0), byte(0), byte(0)
	applyDamage := cfg.Case == CaseDamaged
	wrapCase := cfg.Case == CasePCRWrap
	ccCase := cfg.Case == CaseCCJump
	psiCase := cfg.Case == CasePSIBad
	missingCase := cfg.Case == CaseMissingIDR

	for seq := 0; seq < cfg.Segments; seq++ {
		var slot [PacketsPerSlot][]byte
		put := func(local int, pkt []byte) { slot[local] = pkt }

		if seq == 0 {
			pat := ts.BuildPAT(1, PMTPID)
			patPkts := ts.PacketizeSection(ts.PATPID, pat, 0)
			put(0, patPkts[0])
			pmt := ts.BuildPMT(1, VideoPID, VideoPID, AudioPID)
			pmtPkts := ts.PacketizeSection(PMTPID, pmt, pmtCC)
			put(1, pmtPkts[0])
			pmtCC++
		}
		if seq == 4 && (applyDamage || psiCase) {
			bad := ts.BuildPMT(2, VideoPID, VideoPID, AudioPID)
			bad[len(bad)-1] ^= 0xff
			pkts := ts.PacketizeSection(PMTPID, bad, pmtCC)
			put(0, pkts[0])
			pmtCC++
		}
		if seq == 8 && (applyDamage || psiCase) {
			good := ts.BuildPMT(3, VideoPID, VideoPID, AudioPID)
			pkts := ts.PacketizeSection(PMTPID, good, pmtCC)
			put(0, pkts[0])
			pmtCC++
		}

		tick := uint64(90000 + seq*90000)
		if wrapCase {
			// Start one second before the 33-bit/PCR-base wrap.
			tick = media.PCRMod27/300 - 90000 + uint64(seq)*90000
		}
		pcrRaw := (tick * 300) % media.PCRMod27
		isIDR := true
		if seq == cfg.Segments-1 && ri == 0 && (applyDamage || missingCase) {
			isIDR = false
		}
		nal := videoES(cfg.Seed, ri, seq, isIDR)
		pts := tick
		pes := PES(0xe0, pts, nal)
		vpkt := ts.BuildMediaPacket(VideoPID, videoCC, true, pes, &pcrRaw, isIDR, false)
		if seq == cfg.Segments-1 && ri == 0 && (applyDamage || ccCase) {
			vpkt[3] = (vpkt[3] & 0xf0) | ((videoCC + 2) & 0x0f)
		}
		put(2, vpkt)
		videoCC = (videoCC + 1) & 0x0f

		apes := PES(0xc0, pts+1800, audioES(cfg.Seed, ri, seq))
		put(3, ts.BuildMediaPacket(AudioPID, audioCC, true, apes, nil, false, false))
		audioCC = (audioCC + 1) & 0x0f

		for local := 4; local < PacketsPerSlot; local++ {
			payload := make([]byte, ts.PacketSize-4)
			for j := range payload {
				payload[j] = byte(0xff)
			}
			null := ts.BuildMediaPacket(ts.NullPID, byte(local&0x0f), false, payload, nil, false, false)
			put(local, null)
		}
		for local := 0; local < PacketsPerSlot; local++ {
			if slot[local] == nil {
				// Use null packets for unused PSI positions without changing
				// the fixed 80-packet tactical slot size.
				payload := make([]byte, ts.PacketSize-4)
				for j := range payload {
					payload[j] = 0xff
				}
				slot[local] = ts.BuildMediaPacket(ts.NullPID, byte(local&0x0f), false, payload, nil, false, false)
			}
			stream = append(stream, slot[local]...)
		}
	}
	return stream, nil
}

func videoES(seed, ri, seq int, idr bool) []byte {
	nalType := byte(0x61)
	if idr {
		nalType = 0x65
	}
	b := []byte{0x00, 0x00, 0x00, 0x01, nalType}
	b = append(b, deterministicByte(seed, ri, seq, 0))
	for i := 0; i < 24; i++ {
		b = append(b, deterministicByte(seed, ri, seq, i+1))
	}
	return b
}

func audioES(seed, ri, seq int) []byte {
	b := []byte{0xff, 0xf1, 0x50, 0x80, 0x01, 0x7f, 0xfc}
	for i := 0; i < 12; i++ {
		b = append(b, deterministicByte(seed, ri, seq, 100+i))
	}
	return b
}

func deterministicByte(seed, ri, seq, salt int) byte {
	h := sha256.Sum256([]byte(fmt.Sprintf("avpipe|%d|%d|%d|%d", seed, ri, seq, salt)))
	return h[0]
}

// PES builds a minimal MPEG-2 PES packet with one PTS.
func PES(streamID byte, pts uint64, es []byte) []byte {
	ptsBytes := ts.EncodePTS(pts)
	pes := []byte{0x00, 0x00, 0x01, streamID}
	length := uint16(3 + 5 + len(es))
	pes = append(pes, byte(length>>8), byte(length))
	pes = append(pes, 0x80, 0x80, 0x05)
	pes = append(pes, ptsBytes...)
	pes = append(pes, es...)
	return pes
}

func HashData(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func SourceHash(sources []Source) string {
	h := sha256.New()
	for _, s := range sources {
		h.Write([]byte(s.Rendition))
		h.Write([]byte{0})
		h.Write(s.Data)
	}
	return hex.EncodeToString(h.Sum(nil))
}
