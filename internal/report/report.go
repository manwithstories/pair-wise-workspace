// Package report collects per-PID stream statistics and renders the
// stable sorted text/JSON pre-flight report.
package report

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/asticode/go-astits"

	"tsprobe/internal/program"
	"tsprobe/internal/tsdemux"
)

// StreamStat holds per-PID counters.
type StreamStat struct {
	PID          uint16 `json:"pid"`
	Packets      int64  `json:"packets"`
	PayloadBytes int64  `json:"payload_bytes"`
	CCJumps      int64  `json:"cc_jumps"`
	Lost         int64  `json:"lost_packets"`
	Dup          int64  `json:"dup_packets"`
	Scrambled    int64  `json:"scrambled_packets"`
	FirstPTS     *int64 `json:"first_pts_ms,omitempty"`
	LastPTS      *int64 `json:"last_pts_ms,omitempty"`
}

// Collector observes every demuxed packet and builds per-PID stats.
type Collector struct {
	streams          map[uint16]*StreamStat
	NullPackets      int64
	ScrambledPackets int64
}

func NewCollector() *Collector {
	return &Collector{streams: map[uint16]*StreamStat{}}
}

func (c *Collector) Stat(pid uint16) *StreamStat {
	return c.streams[pid]
}

func (c *Collector) Observe(p *tsdemux.Packet) {
	st := c.streams[p.PID]
	if st == nil {
		st = &StreamStat{PID: p.PID}
		c.streams[p.PID] = st
	}
	st.Packets++
	if p.PID == tsdemux.NullPID {
		c.NullPackets++
		return
	}
	if p.Scrambled {
		st.Scrambled++
		c.ScrambledPackets++
		return
	}
	if p.TEI || !p.HasPayload {
		return
	}
	st.PayloadBytes += int64(len(p.Payload))
	if p.PUSI && len(p.Payload) >= 6 &&
		p.Payload[0] == 0 && p.Payload[1] == 0 && p.Payload[2] == 1 {
		if ms, ok := parsePTS(p.Payload); ok {
			if st.FirstPTS == nil {
				first := ms
				st.FirstPTS = &first
			}
			st.LastPTS = &ms
		}
	}
}

// parsePTS extracts the PTS (in milliseconds) of a PES payload using
// go-astits for the low level TS packet and PES header parsing. The
// payload is wrapped into a synthetic TS packet so the astits demuxer
// can read the PES header.
func parsePTS(payload []byte) (int64, bool) {
	// Two packets: the PES packet plus a null packet so the astits
	// demuxer can auto detect the packet size.
	pkt := make([]byte, 2*tsdemux.PacketSize188)
	pkt[0] = 0x47
	pkt[1] = 0x41 // PUSI, PID 0x100 (PID 0 would be parsed as PAT)
	pkt[2] = 0x00
	pkt[3] = 0x10
	n := copy(pkt[4:], payload)
	for i := 4 + n; i < len(pkt); i++ {
		pkt[i] = 0xFF
	}
	pkt[tsdemux.PacketSize188] = 0x47
	pkt[tsdemux.PacketSize188+1] = 0x1F
	pkt[tsdemux.PacketSize188+2] = 0xFF
	pkt[tsdemux.PacketSize188+3] = 0x10
	dmx := astits.NewDemuxer(context.Background(), bytes.NewReader(pkt))
	for {
		d, err := dmx.NextData()
		if err != nil {
			return 0, false
		}
		if d.PES != nil && d.PES.Header != nil && d.PES.Header.OptionalHeader != nil &&
			d.PES.Header.OptionalHeader.PTS != nil {
			return d.PES.Header.OptionalHeader.PTS.Base / 90, true
		}
	}
}

// Report is the full pre-flight result.
type Report struct {
	Input             string                `json:"input"`
	PacketCount       int64                 `json:"packet_count"`
	PacketSize        int                   `json:"packet_size"`
	SyncLossEvents    int64                 `json:"sync_loss_events"`
	TailBytes         int                   `json:"tail_bytes"`
	TEIPackets        int64                 `json:"tei_packets"`
	CRCErrors         int64                 `json:"crc_errors"`
	PATVersionChanges int                   `json:"pat_version_changes"`
	PMTVersionChanges map[string]int        `json:"pmt_version_changes"`
	PIDMap            []program.PIDMapEntry `json:"pid_map"`
	CCJumps           int64                 `json:"cc_jumps"`
	LostPackets       int64                 `json:"lost_packets"`
	DupPackets        int64                 `json:"dup_packets"`
	NullPackets       int64                 `json:"null_packets"`
	ScrambledPackets  int64                 `json:"scrambled_packets"`
	Streams           []StreamStat          `json:"streams"`
	Warnings          []string              `json:"warnings,omitempty"`
}

// Build assembles the report from the pipeline components.
func Build(input string, dm *tsdemux.Demuxer, cc *tsdemux.CCTracker,
	col *Collector, st *program.State, crcErrors int64) *Report {
	r := &Report{
		Input:             input,
		PacketCount:       dm.Stats.Packets,
		PacketSize:        dm.Stats.PacketSize,
		SyncLossEvents:    dm.Stats.SyncLoss,
		TailBytes:         dm.Stats.TailBytes,
		TEIPackets:        dm.Stats.TEI,
		CRCErrors:         crcErrors,
		PATVersionChanges: st.PATChanges,
		PMTVersionChanges: map[string]int{},
		PIDMap:            st.PIDMap,
		NullPackets:       col.NullPackets,
		ScrambledPackets:  col.ScrambledPackets,
	}
	for prog, n := range st.PMTChanges {
		r.PMTVersionChanges[fmt.Sprintf("%d", prog)] = n
	}
	jumps, lost, dup := cc.Totals()
	r.CCJumps, r.LostPackets, r.DupPackets = jumps, lost, dup
	for pid, s := range col.streams {
		s.CCJumps, s.Lost, s.Dup = cc.Stats(pid)
		r.Streams = append(r.Streams, *s)
	}
	sort.Slice(r.Streams, func(i, j int) bool { return r.Streams[i].PID < r.Streams[j].PID })
	if r.TailBytes > 0 {
		r.Warnings = append(r.Warnings,
			fmt.Sprintf("ignored %d trailing bytes not forming a full packet", r.TailBytes))
	}
	if r.SyncLossEvents > 0 {
		r.Warnings = append(r.Warnings,
			fmt.Sprintf("lost sync %d time(s), resynchronised", r.SyncLossEvents))
	}
	return r
}

// JSON writes the report as indented JSON.
func (r *Report) JSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// Text writes a human readable, stably sorted report.
func (r *Report) Text(w io.Writer) {
	fmt.Fprintf(w, "input: %s\n", r.Input)
	fmt.Fprintf(w, "packet_count: %d (packet_size %d)\n", r.PacketCount, r.PacketSize)
	fmt.Fprintf(w, "sync_loss_events: %d  tail_bytes: %d  tei_packets: %d  crc_errors: %d\n",
		r.SyncLossEvents, r.TailBytes, r.TEIPackets, r.CRCErrors)
	fmt.Fprintf(w, "pat_version_changes: %d\n", r.PATVersionChanges)
	progs := make([]string, 0, len(r.PMTVersionChanges))
	for p := range r.PMTVersionChanges {
		progs = append(progs, p)
	}
	sort.Strings(progs)
	for _, p := range progs {
		fmt.Fprintf(w, "pmt_version_changes program %s: %d\n", p, r.PMTVersionChanges[p])
	}
	fmt.Fprintf(w, "cc_jumps: %d  lost_packets: %d  dup_packets: %d\n",
		r.CCJumps, r.LostPackets, r.DupPackets)
	fmt.Fprintf(w, "null_packets: %d  scrambled_packets: %d\n",
		r.NullPackets, r.ScrambledPackets)
	fmt.Fprintf(w, "pid_map (%d entries):\n", len(r.PIDMap))
	for _, e := range r.PIDMap {
		fmt.Fprintf(w, "  program %d pmt_pid 0x%04X type 0x%02X pid 0x%04X @packet %d (%s)\n",
			e.Program, e.PMTPID, e.StreamType, e.PID, e.Packet, e.Note)
	}
	fmt.Fprintf(w, "streams (%d):\n", len(r.Streams))
	for _, s := range r.Streams {
		fmt.Fprintf(w, "  pid 0x%04X packets %d payload_bytes %d cc_jumps %d lost %d dup %d",
			s.PID, s.Packets, s.PayloadBytes, s.CCJumps, s.Lost, s.Dup)
		if s.FirstPTS != nil {
			fmt.Fprintf(w, " pts %d..%d ms", *s.FirstPTS, *s.LastPTS)
		}
		fmt.Fprintln(w)
	}
	for _, warn := range r.Warnings {
		fmt.Fprintf(w, "warning: %s\n", warn)
	}
}
