package mocksrv

import (
	"encoding/binary"
	"math/rand"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph264"
	"github.com/pion/rtp"
)

// streamer generates the synthetic H.264 stream, injects the configured
// faults and paces the packets onto the negotiated channels.
type streamer struct {
	cfg     Config
	st      *connState
	nc      net.Conn
	stopCh  chan struct{}
	doneCh  chan struct{}
	stopOne sync.Once
}

func newStreamer(cfg Config, st *connState, nc net.Conn) *streamer {
	return &streamer{
		cfg:    cfg,
		st:     st,
		nc:     nc,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
}

func (st *streamer) start() { go st.run() }

func (st *streamer) stop() {
	st.stopOne.Do(func() {
		close(st.stopCh)
		<-st.doneCh
	})
}

// run builds the per-track packet lists, applies faults, merges them
// round-robin and sends at the configured pace.
func (st *streamer) run() {
	defer close(st.doneCh)
	idxs := make([]int, 0, len(st.st.tracks))
	for idx := range st.st.tracks {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	if len(idxs) == 0 {
		return
	}

	perTrack := make(map[int][]*rtp.Packet, len(idxs))
	for pos, idx := range idxs {
		count := st.cfg.Packets / len(idxs)
		if pos == 0 {
			count += st.cfg.Packets % len(idxs)
		}
		pkts := genTrackPackets(&st.cfg, idx, count)
		// Fault budgets are global; split them across tracks so totals
		// match the requested drop/dup/reorder counts.
		f := faultBudget{
			drop:    split(st.cfg.Drop, len(idxs), pos),
			dup:     split(st.cfg.Dup, len(idxs), pos),
			reorder: split(st.cfg.Reorder, len(idxs), pos),
		}
		perTrack[idx] = applyFaults(pkts, f)
	}

	total := 0
	for _, pkts := range perTrack {
		total += len(pkts)
	}
	interval := time.Second / time.Duration(st.cfg.Rate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	cursors := make(map[int]int, len(idxs))
	for sent := 0; sent < total; {
		for _, idx := range idxs {
			c := cursors[idx]
			if c >= len(perTrack[idx]) {
				continue
			}
			select {
			case <-st.stopCh:
				return
			case <-ticker.C:
			}
			st.send(st.st.tracks[idx], perTrack[idx][c])
			cursors[idx] = c + 1
			sent++
		}
	}
}

// send writes one packet on its negotiated channel.
func (st *streamer) send(ts *trackSetup, pkt *rtp.Packet) {
	raw, err := pkt.Marshal()
	if err != nil {
		return
	}
	if ts.interleaved {
		frame := make([]byte, 4+len(raw))
		frame[0] = 0x24
		frame[1] = byte(ts.channel)
		binary.BigEndian.PutUint16(frame[2:], uint16(len(raw)))
		copy(frame[4:], raw)
		st.st.writeMu.Lock()
		_, _ = st.nc.Write(frame)
		st.st.writeMu.Unlock()
		return
	}
	_, _ = ts.udpConn.WriteToUDP(raw, ts.udpDst)
}

// genTrackPackets builds count RTP packets of synthetic H.264 for one
// track. Sequence numbers are contiguous per track; timestamps advance per
// access unit; the marker bit closes each AU.
func genTrackPackets(cfg *Config, trackIdx, count int) []*rtp.Packet {
	enc := &rtph264.Encoder{PayloadType: uint8(96 + trackIdx)}
	if err := enc.Init(); err != nil {
		return nil
	}
	rng := rand.New(rand.NewSource(cfg.Seed + int64(trackIdx)*7919))
	var out []*rtp.Packet
	frame := 0
	for len(out) < count {
		au := make([][]byte, cfg.PacketsPerFrame)
		for i := range au {
			au[i] = make([]byte, 800+rng.Intn(200)) // >728B: never STAP-A aggregated
			if i == 0 {
				au[i][0] = 0x65 // IDR
			} else {
				au[i][0] = 0x41 // non-IDR
			}
			for j := 1; j < len(au[i]); j++ {
				au[i][j] = byte(rng.Intn(256))
			}
		}
		pkts, err := enc.Encode(au)
		if err != nil {
			return nil
		}
		for _, p := range pkts {
			p.Timestamp = 1000 + uint32(frame*3000)
			out = append(out, p)
			if len(out) == count {
				break
			}
		}
		frame++
	}
	for i, p := range out {
		p.SequenceNumber = uint16(1000 + i)
	}
	return out
}

// faultBudget is the per-track share of the global fault counts.
type faultBudget struct {
	drop    int
	dup     int
	reorder int
}

// split distributes total across n tracks; earlier tracks get the remainder.
func split(total, n, pos int) int {
	q := total / n
	if pos < total%n {
		q++
	}
	return q
}

// applyFaults drops, duplicates and swaps packets at deterministic,
// evenly spread positions kept away from the stream tail so the client
// can always observe the resulting gaps.
func applyFaults(pkts []*rtp.Packet, f faultBudget) []*rtp.Packet {
	n := len(pkts)
	if n == 0 {
		return pkts
	}
	drops := spread(f.drop, n, 0)
	dups := spread(f.dup, n, 3)
	swaps := spread(f.reorder, n, 7)

	list := make([]*rtp.Packet, n)
	copy(list, pkts)
	for pos := range swaps {
		if pos+1 < n && !drops[pos] && !drops[pos+1] {
			list[pos], list[pos+1] = list[pos+1], list[pos]
		}
	}
	out := make([]*rtp.Packet, 0, n+f.dup)
	for i, p := range list {
		if drops[i] {
			continue
		}
		out = append(out, p)
		if dups[i] {
			out = append(out, p)
		}
	}
	return out
}

// spread picks k evenly spaced positions in [0, n), offset by a per-fault
// salt so drop/dup/reorder never collide.
func spread(k, n, salt int) map[int]bool {
	out := make(map[int]bool, k)
	if k <= 0 {
		return out
	}
	step := n / (k + 1)
	if step < 1 {
		step = 1
	}
	for i := 0; i < k; i++ {
		pos := (step*(i+1) + salt) % n
		for out[pos] {
			pos = (pos + 1) % n
		}
		out[pos] = true
	}
	return out
}
