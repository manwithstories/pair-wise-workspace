// Package report defines the audit result and its text/JSON renderers.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"tsaudit/internal/counter"
	"tsaudit/internal/psi"
	"tsaudit/internal/segment"
)

type Report struct {
	Input             string            `json:"input,omitempty"`
	Packets           int64             `json:"packets"`
	Bytes             int64             `json:"bytes"`
	SyncErrors        int64             `json:"sync_errors"`
	ResyncLostBytes   int64             `json:"resync_lost_bytes"`
	TransportErrors   int64             `json:"transport_errors"`
	CRCFailures       int               `json:"crc_failures"`
	DuplicateSections int               `json:"duplicate_sections"`
	PATVersionChanges int               `json:"pat_version_changes"`
	PMTVersionChanges map[uint16]int    `json:"pmt_version_changes"`
	PIDMap            []psi.PIDEntry    `json:"pid_map"`
	ESPacketsTotal    int64             `json:"es_packets_total"`
	CCJumps           int               `json:"cc_jumps"`
	CCJumpEvents      []counter.Jump    `json:"cc_jump_events"`
	CCDuplicates      int               `json:"cc_duplicates"`
	LostPackets       int               `json:"lost_packets"`
	Segments          int               `json:"segments"`
	SliceManifest     []segment.Segment `json:"slice_manifest"`
	VersionChanges    []psi.Change      `json:"version_changes"`
	Conflicts         []psi.Conflict    `json:"conflicts"`
	Warnings          []string          `json:"warnings"`
	Success           bool              `json:"success"`
}

func (r *Report) RenderJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func (r *Report) RenderText(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "input: %s\npackets: %d\nbytes: %d\n", r.Input, r.Packets, r.Bytes); err != nil {
		return err
	}
	fmt.Fprintf(w, "sync_errors: %d transport_errors: %d crc_failures: %d duplicate_sections: %d\n",
		r.SyncErrors, r.TransportErrors, r.CRCFailures, r.DuplicateSections)
	fmt.Fprintf(w, "pat_version_changes=%d\n", r.PATVersionChanges)
	fmt.Fprint(w, "pmt_version_changes:")
	keys := sortedProgramKeys(r.PMTVersionChanges)
	for _, program := range keys {
		fmt.Fprintf(w, " program_%d=%d", program, r.PMTVersionChanges[program])
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "pid_map:")
	for _, e := range r.PIDMap {
		role := e.Role
		if e.Program != 0 {
			role = fmt.Sprintf("%s/program=%d", role, e.Program)
		}
		extra := ""
		if e.StreamType != nil {
			extra = fmt.Sprintf(" stream_type=0x%02x", *e.StreamType)
		}
		if e.PacketCount > 0 {
			extra += fmt.Sprintf(" packet_count=%d", e.PacketCount)
		}
		fmt.Fprintf(w, "  PID 0x%04x %-18s%s\n", e.PID, role, extra)
	}
	fmt.Fprintf(w, "es_packets_total=%d\n", r.ESPacketsTotal)
	fmt.Fprintf(w, "cc_jumps=%d cc_duplicates=%d lost_packets=%d\n", r.CCJumps, r.CCDuplicates, r.LostPackets)
	for _, j := range r.CCJumpEvents {
		fmt.Fprintf(w, "  jump offset=%d PID=0x%04x %d->%d lost=%d snapshot=%d\n",
			j.Offset, j.PID, j.Previous, j.Next, j.Lost, j.SnapshotID)
	}
	fmt.Fprintf(w, "segments=%d\n", r.Segments)
	for _, seg := range r.SliceManifest {
		fmt.Fprintf(w, "  segment-%03d bytes=[%d,%d) snapshot=%d boundary=%s\n",
			seg.Index, seg.StartByte, seg.EndByte, seg.SnapshotID, seg.Boundary)
	}
	if len(r.Conflicts) > 0 {
		fmt.Fprintln(w, "conflicts:")
		for _, c := range r.Conflicts {
			fmt.Fprintf(w, "  snapshot=%d PID=0x%04x program=%d %s\n", c.SnapshotID, c.PID, c.Program, c.Message)
		}
	}
	if len(r.Warnings) > 0 {
		fmt.Fprintln(w, "warnings:")
		for _, warning := range r.Warnings {
			fmt.Fprintf(w, "  %s\n", warning)
		}
	}
	fmt.Fprintf(w, "success=%t\n", r.Success)
	return nil
}

func sortedProgramKeys(m map[uint16]int) []uint16 {
	out := make([]uint16, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
