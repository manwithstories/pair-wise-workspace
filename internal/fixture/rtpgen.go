// RTP trace generation: produces recorded packet streams with controlled
// duplicates, reordering, burst loss and wrap-around. Traces are gob-encoded
// locally, so the demo never needs a network or an external capture file.
package fixture

import (
	"encoding/gob"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"archive.local/archive/internal/rtp"
)

// TracePlan describes the transport defects to inject into a generated trace.
type TracePlan struct {
	Packets      int
	Payload      int
	DupEvery     int     // inject a duplicate every N accepted packets (0 = off)
	ReorderEvery int     // swap with the next packet every N (0 = off)
	Bursts       []Burst // injected loss bursts
	StartSeq     uint16  // initial wire sequence
	StartTS      uint32
}

// Burst is one injected loss event.
type Burst struct {
	At    int // index in the packet stream where loss begins
	Count int // packets missing
}

// Trace is the recorded packet stream plus what was injected, so tests can
// assert the tracker recovers the defects exactly.
type Trace struct {
	Packets         []rtp.Packet
	InjectedDup     int
	InjectedReorder int
	InjectedLoss    int
	SSRC            uint32
}

// GenerateTrace produces a deterministic trace driven by seed.
func GenerateTrace(path string, plan TracePlan, seed int64) (*Trace, error) {
	if plan.Packets <= 0 {
		return nil, fmt.Errorf("fixture: trace needs packets")
	}
	if plan.Payload <= 0 {
		plan.Payload = 200
	}
	if plan.StartSeq == 0 {
		plan.StartSeq = 40000 // so a wrap can be observed naturally
	}
	rng := rand.New(rand.NewSource(seed))
	tr := &Trace{SSRC: uint32(0xA11CE + seed%97)}
	seq := plan.StartSeq
	ts := plan.StartTS
	lossLeft := 0
	burstIdx := 0
	for i := 0; i < plan.Packets; i++ {
		if burstIdx < len(plan.Bursts) && i >= plan.Bursts[burstIdx].At {
			lossLeft += plan.Bursts[burstIdx].Count
			burstIdx++
		}
		if lossLeft > 0 {
			lossLeft--
			tr.InjectedLoss++
			seq++ // the wire sequence advances even though nothing arrived
			ts += 3000
			i--
			continue
		}
		p := rtp.Packet{Seq: seq, Timestamp: ts, SSRC: tr.SSRC,
			Payload: make([]byte, plan.Payload)}
		for k := range p.Payload {
			p.Payload[k] = byte((i*13 + k*7 + int(seed)) % 251)
		}
		wire, err := p.Marshal()
		if err != nil {
			return nil, err
		}
		p.Wire = wire
		tr.Packets = append(tr.Packets, p)
		if plan.DupEvery > 0 && (i+1)%plan.DupEvery == 0 {
			// A duplicate delivered later in the trace.
			tr.Packets = append(tr.Packets, p)
			tr.InjectedDup++
		}
		seq++
		ts += 3000
		// Reordering: delay one packet by emitting the next one first.
		if plan.ReorderEvery > 0 && (i+1)%plan.ReorderEvery == 0 && i+2 < plan.Packets {
			next := rtp.Packet{Seq: seq + 1, Timestamp: ts + 3000, SSRC: tr.SSRC,
				Payload: make([]byte, plan.Payload)}
			for k := range next.Payload {
				next.Payload[k] = byte((int(seed) + k*11) % 251)
			}
			nwire, err := next.Marshal()
			if err != nil {
				return nil, err
			}
			next.Wire = nwire
			late := p2(seq, ts, tr.SSRC, plan.Payload, seed)
			lwire, err := late.Marshal()
			if err != nil {
				return nil, err
			}
			late.Wire = lwire
			tr.Packets = append(tr.Packets, next)
			tr.Packets = append(tr.Packets, late)
			tr.InjectedReorder++
			i++
			seq += 2
			ts += 6000
		}
		_ = rng
	}
	if err := writeTrace(path, tr); err != nil {
		return nil, err
	}
	return tr, nil
}

func p2(seq uint16, ts uint32, ssrc uint32, payload int, seed int64) rtp.Packet {
	p := rtp.Packet{Seq: seq, Timestamp: ts, SSRC: ssrc, Payload: make([]byte, payload)}
	for k := range p.Payload {
		p.Payload[k] = byte((int(seed) + k*3) % 251)
	}
	return p
}

// writeTrace gob-encodes a trace to disk with owner-only permissions.
func writeTrace(path string, tr *Trace) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	return gob.NewEncoder(f).Encode(tr)
}

// ReadTrace loads a trace written by GenerateTrace or the demo.
func ReadTrace(path string) (*Trace, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var tr Trace
	if err := gob.NewDecoder(f).Decode(&tr); err != nil {
		return nil, fmt.Errorf("fixture: decode trace: %w", err)
	}
	return &tr, nil
}
