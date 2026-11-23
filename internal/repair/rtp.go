// Package repair analyses an RTP stream for media preparation. It reorders
// out-of-order packets, drops duplicates, restores payloads that are still
// carried as a redundant copy in the following packet, and reports the
// intervals that cannot be recovered together with the residual loss rate.
package repair

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
)

// Errors returned by analysis.
var (
	ErrShortPayload  = errors.New("rtp payload too short")
	ErrBadRedundancy = errors.New("malformed redundancy block")
)

// Gap is a run of consecutive missing sequence numbers that could not be
// reconstructed.
type Gap struct {
	Start uint16
	End   uint16
	Count int
}

// String renders the gap for the terminal summary.
func (g Gap) String() string {
	if g.Count == 1 {
		return fmt.Sprintf("%d", g.Start)
	}
	return fmt.Sprintf("%d-%d", g.Start, g.End)
}

// Result is the outcome of analysing one RTP stream.
type Result struct {
	Received    int
	Expected    int
	Duplicates  int
	Reordered   int
	Corrupt     int
	Recovered   int
	Lost        int
	Gaps        []Gap
	Payloads    []Payload
	Warnings    []string
	LossPct     float64 // residual loss after recovery
	RecoveryPct float64 // recovered / (recovered + lost)
	Digest      string
}

// Payload is one reconstructed media unit in presentation order.
type Payload struct {
	Seq       uint16
	Timestamp uint32
	Bytes     []byte
	Recovered bool
}

// Analyze reconstructs the stream. The result is deterministic for a given
// set of datagrams because sequence order, not arrival order, defines output.
func Analyze(raws [][]byte) Result {
	res := Result{
		Payloads: make([]Payload, 0, len(raws)),
		Warnings: make([]string, 0, 4),
	}
	type candidate struct {
		pkt    *Packet
		ext    int64
		arrive int
	}
	bySeq := make(map[int64]*candidate, len(raws))
	order := make([]int64, 0, len(raws))
	var base int64
	haveBase := false
	var lastExt int64
	maxExt := int64(-1 << 62)
	minExt := int64(1 << 62)
	arrival := 0

	for _, raw := range raws {
		arrival++
		pkt, err := Decode(raw)
		if err != nil {
			res.Corrupt++
			res.Warnings = append(res.Warnings, fmt.Sprintf("corrupt packet #%d: %v", arrival, err))
			continue
		}
		ext := int64(pkt.Seq)
		if haveBase {
			// Extend the 16-bit counter into a monotonic int64 by choosing the
			// epoch nearest the previously seen sequence number.
			ext = lastExt - (lastExt & 0xffff) + int64(pkt.Seq)
			for ext-lastExt > 0x8000 {
				ext -= 0x10000
			}
			for lastExt-ext > 0x8000 {
				ext += 0x10000
			}
		} else {
			base = int64(pkt.Seq)
			haveBase = true
			ext = base
			lastExt = ext
		}
		if ext < lastExt {
			res.Reordered++
		}
		lastExt = ext
		if ext > maxExt {
			maxExt = ext
		}
		if ext < minExt {
			minExt = ext
		}
		res.Received++
		if prev, ok := bySeq[ext]; ok {
			res.Duplicates++
			_ = prev
			continue
		}
		bySeq[ext] = &candidate{pkt: pkt, ext: ext, arrive: arrival}
		order = append(order, ext)
	}

	if len(bySeq) == 0 {
		res.LossPct = 100
		res.Digest = digestPayloads(res.Payloads)
		return res
	}

	res.Expected = int(maxExt-minExt) + 1
	recovered := make(map[int64]bool)
	missing := make([]int64, 0, 8)

	for ext := minExt; ext <= maxExt; ext++ {
		if c, ok := bySeq[ext]; ok {
			res.Payloads = append(res.Payloads, Payload{
				Seq:       c.pkt.Seq,
				Timestamp: c.pkt.Timestamp,
				Bytes:     c.pkt.Payload,
			})
			continue
		}
		// Try to restore from the next packet's redundant copy.
		if next, ok := bySeq[ext+1]; ok && next.pkt.Redundant != nil &&
			next.pkt.Redundant.Seq == uint16Of(ext) {
			res.Payloads = append(res.Payloads, Payload{
				Seq:       next.pkt.Redundant.Seq,
				Timestamp: next.pkt.Timestamp,
				Bytes:     next.pkt.Redundant.Bytes,
				Recovered: true,
			})
			recovered[ext] = true
			res.Recovered++
			continue
		}
		missing = append(missing, ext)
	}

	sort.SliceStable(res.Payloads, func(i, j int) bool { return res.Payloads[i].Seq < res.Payloads[j].Seq })
	res.Gaps = collectGaps(missing)
	res.Lost = len(missing)
	if res.Expected > 0 {
		res.LossPct = float64(res.Lost) / float64(res.Expected) * 100
	}
	denom := res.Recovered + res.Lost
	if denom > 0 {
		res.RecoveryPct = float64(res.Recovered) / float64(denom) * 100
	} else {
		res.RecoveryPct = 100
	}
	res.Digest = digestPayloads(res.Payloads)
	return res
}

func uint16Of(ext int64) uint16 { return uint16(ext & 0xffff) }

func collectGaps(missing []int64) []Gap {
	var gaps []Gap
	for i := 0; i < len(missing); {
		start := missing[i]
		end := start
		for i+1 < len(missing) && missing[i+1] == end+1 {
			i++
			end = missing[i]
		}
		gaps = append(gaps, Gap{Start: uint16Of(start), End: uint16Of(end), Count: int(end-start) + 1})
		i++
	}
	return gaps
}

func digestPayloads(ps []Payload) string {
	h := sha256.New()
	for _, p := range ps {
		var hdr [8]byte
		binary.BigEndian.PutUint16(hdr[0:2], p.Seq)
		binary.BigEndian.PutUint32(hdr[2:6], p.Timestamp)
		h.Write(hdr[:6])
		if p.Recovered {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
		}
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(p.Bytes)))
		h.Write(n[:])
		h.Write(p.Bytes)
	}
	return hex.EncodeToString(h.Sum(nil))
}
