package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"strconv"
	"strings"

	"github.com/pion/rtp"

	"mediaroute/internal/recovery"
)

// Params pins every input of a demo run so results are deterministic.
type Params struct {
	Seed           int64
	Bandwidth      []int64 // bps phases
	DropStart      int
	DropCount      int
	Duplicates     int
	Reorder        int
	CorruptSegment int // -1 = none
	Segments       int
	PktsPerSeg     int
}

func defaultParams() Params {
	return Params{Seed: 41, Bandwidth: []int64{2_000_000, 700_000, 3_000_000},
		DropStart: 18, DropCount: 6, Duplicates: 9, Reorder: 12,
		CorruptSegment: -1, Segments: 8, PktsPerSeg: 8}
}

// Canonical is the stable string the input digest is computed from.
func (p Params) Canonical() string {
	bw := make([]string, len(p.Bandwidth))
	for i, b := range p.Bandwidth {
		bw[i] = strconv.FormatInt(b, 10)
	}
	return fmt.Sprintf("seed=%d|bw=%s|drop=%d:%d|dup=%d|reorder=%d|corrupt=%d|seg=%d|pps=%d",
		p.Seed, strings.Join(bw, ","), p.DropStart, p.DropCount,
		p.Duplicates, p.Reorder, p.CorruptSegment, p.Segments, p.PktsPerSeg)
}

// Digest is the input summary stored in checkpoints.
func (p Params) Digest() string {
	sum := sha256.Sum256([]byte(p.Canonical()))
	return hex.EncodeToString(sum[:])
}

// parseBandwidth parses "2m,700k,3m" into bps phases.
func parseBandwidth(s string) ([]int64, error) {
	var out []int64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		mult := int64(1)
		switch {
		case strings.HasSuffix(part, "m"):
			mult, part = 1_000_000, part[:len(part)-1]
		case strings.HasSuffix(part, "k"):
			mult, part = 1_000, part[:len(part)-1]
		}
		v, err := strconv.ParseFloat(part, 64)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("bad bandwidth value %q", part)
		}
		out = append(out, int64(v*float64(mult)))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty bandwidth spec")
	}
	return out, nil
}

const (
	videoSSRC = 1001
	audioSSRC = 1002
	videoStep = 3600 // 90 kHz, 40 ms per packet
	audioStep = 1920 // 48 kHz, 40 ms per packet
	videoSkew = 3150 // video presented 35 ms early (90 kHz units)
)

// World is the deterministic generated input of one demo run.
type World struct {
	Params
	Playlist      string
	VideoArrival  []rtp.Packet
	AudioArrival  []rtp.Packet
	Parity        []recovery.Parity
	VideoPayloads [][]byte // clean payloads by seq, for segment assembly
	AudioPayloads [][]byte
}

func videoTS(seq int) uint32 { return uint32(seq*videoStep + videoSkew) }
func audioTS(seq int) uint32 { return uint32(seq * audioStep) }

// GenWorld builds the deterministic fixture: clean packets, FEC parity
// and the corrupted arrival order (drop burst, reorder, duplicates).
func GenWorld(p Params) *World {
	rng := rand.New(rand.NewSource(p.Seed))
	n := p.Segments * p.PktsPerSeg
	w := &World{Params: p}
	mk := func(ssrc uint32, seq int, ts uint32, size int) rtp.Packet {
		pl := make([]byte, size)
		rng.Read(pl)
		return rtp.Packet{Header: rtp.Header{Version: 2, SSRC: ssrc,
			SequenceNumber: uint16(seq), Timestamp: ts}, Payload: pl}
	}
	var video, audio []rtp.Packet
	for i := 0; i < n; i++ {
		video = append(video, mk(videoSSRC, i, videoTS(i), 120))
	}
	for i := 0; i < n; i++ {
		audio = append(audio, mk(audioSSRC, i, audioTS(i), 60))
	}
	for _, pkt := range video {
		w.VideoPayloads = append(w.VideoPayloads, pkt.Payload)
	}
	for _, pkt := range audio {
		w.AudioPayloads = append(w.AudioPayloads, pkt.Payload)
	}
	for i := 0; i < n; i++ { // parity covers seq%16<8, riding outside any burst
		if i%16 >= 8 {
			continue
		}
		w.Parity = append(w.Parity, recovery.Parity{
			Protects: uint64(i), Timestamp: videoTS(i), Payload: video[i].Payload})
	}
	// arrival order: drop burst, then swap Reorder adjacent pairs,
	// then duplicate Duplicates packets.
	var arrival []rtp.Packet
	for i := 0; i < n; i++ {
		if i >= p.DropStart && i < p.DropStart+p.DropCount {
			continue
		}
		arrival = append(arrival, video[i])
	}
	var re []rtp.Packet
	for i := 0; i < len(arrival); i++ {
		k := i / 2
		if k < p.Reorder && i%2 == 0 && i+1 < len(arrival) {
			re = append(re, arrival[i+1], arrival[i])
			i++
			continue
		}
		re = append(re, arrival[i])
	}
	dupFrom := p.DropStart + p.DropCount + 2*p.Reorder
	var arr []rtp.Packet
	for _, pkt := range re {
		arr = append(arr, pkt)
		s := int(pkt.SequenceNumber)
		if s >= dupFrom && s < dupFrom+p.Duplicates {
			arr = append(arr, pkt) // identical retransmission
		}
	}
	w.VideoArrival = arr
	w.AudioArrival = audio
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n")
	for i := 0; i < p.Segments; i++ {
		fmt.Fprintf(&b, "#EXTINF:0.320,\nseg-%d.m4s\n", i)
	}
	w.Playlist = b.String()
	return w
}
