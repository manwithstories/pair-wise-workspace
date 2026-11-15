// Package report converts a scan result into deterministic JSON and terminal tables.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"

	"tsprobe/internal/demux"
	"tsprobe/internal/psi"
)

// SDTReport is the current service-description summary.
type SDTReport struct {
	Version  *uint8        `json:"version"`
	Services []psi.Service `json:"services"`
}

// TableReport summarises one PAT or PMT PID.
type TableReport struct {
	Table          string              `json:"table"`
	PID            uint16              `json:"pid"`
	ProgramNumber  uint16              `json:"program_number,omitempty"`
	VersionChanges int                 `json:"version_changes"`
	FirstVersion   *uint8              `json:"first_version"`
	LastVersion    *uint8              `json:"last_version"`
	Stale          bool                `json:"stale"`
	Active         bool                `json:"active"`
	Mapping        psi.MappingSnapshot `json:"mapping"`
	Changes        []psi.VersionEvent  `json:"changes"`
}

// Report is the stable scan document written by --json.
type Report struct {
	File           string              `json:"file"`
	SizeBytes      int64               `json:"size_bytes"`
	PacketsTotal   uint64              `json:"packets_total"`
	DurationMS     int64               `json:"duration_ms"`
	Anomalies      demux.Anomalies     `json:"anomalies"`
	PAT            TableReport         `json:"pat"`
	PMT            []TableReport       `json:"pmt"`
	Streams        []demux.StreamStats `json:"streams"`
	NullPID        demux.NullStats     `json:"null_pid"`
	CCGaps         []demux.CCGap       `json:"cc_gaps"`
	LostPackets    uint64              `json:"lost_packets"`
	Stale          bool                `json:"stale"`
	MappingHistory []psi.MappingPoint  `json:"mapping_history"`
	SDT            SDTReport           `json:"sdt"`
}

// Build creates a report from a shared demux scan result.
func Build(r *demux.Result) Report {
	events := append([]psi.VersionEvent(nil), r.Store.Events()...)
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].PacketIndex != events[j].PacketIndex {
			return events[i].PacketIndex < events[j].PacketIndex
		}
		if events[i].PID != events[j].PID {
			return events[i].PID < events[j].PID
		}
		return events[i].Table < events[j].Table
	})
	rep := Report{
		File: r.Path, SizeBytes: r.SizeBytes, PacketsTotal: r.PacketsTotal, DurationMS: r.DurationMS,
		Anomalies: r.Anomalies, Streams: r.Streams, NullPID: r.Null, CCGaps: r.CCGaps,
		LostPackets: r.LostPackets, Stale: r.Store.Stale(), MappingHistory: r.Store.MappingPoints(),
	}
	rep.PAT = tableReport(r.Store.PATInfo(), events)
	for _, info := range r.Store.PMTInfos() {
		rep.PMT = append(rep.PMT, tableReport(info, events))
	}
	sort.Slice(rep.PMT, func(i, j int) bool { return rep.PMT[i].PID < rep.PMT[j].PID })
	if sdt := r.Store.SDT(); sdt != nil {
		v := sdt.Version
		rep.SDT.Version = &v
		rep.SDT.Services = sdt.Services
	}
	if rep.PMT == nil {
		rep.PMT = []TableReport{}
	}
	if rep.MappingHistory == nil {
		rep.MappingHistory = []psi.MappingPoint{}
	}
	if rep.CCGaps == nil {
		rep.CCGaps = []demux.CCGap{}
	}
	if rep.Streams == nil {
		rep.Streams = []demux.StreamStats{}
	}
	return rep
}

func tableReport(info psi.TableInfo, events []psi.VersionEvent) TableReport {
	tr := TableReport{
		Table: info.Table, PID: info.PID, ProgramNumber: info.ProgramNumber,
		VersionChanges: info.VersionChanges, Stale: info.Stale, Active: info.Active,
		Mapping: info.Mapping, Changes: []psi.VersionEvent{},
	}
	if info.HasVersion {
		first, last := info.FirstVersion, info.LastVersion
		tr.FirstVersion, tr.LastVersion = &first, &last
	}
	for _, e := range events {
		if e.Table == info.Table && e.PID == info.PID {
			tr.Changes = append(tr.Changes, e)
		}
	}
	return tr
}

// WriteJSON writes indented deterministic JSON atomically enough for local CLI use.
func WriteJSON(path string, rep Report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	bs, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	bs = append(bs, '\n')
	return os.WriteFile(path, bs, 0o644)
}

// NeedsExitTwo reports whether the file is complete but has observable anomalies.
func (r Report) NeedsExitTwo() bool {
	if r.LostPackets > 0 || r.Stale || r.PAT.VersionChanges > 0 || hasAnomalies(r.Anomalies) {
		return true
	}
	for _, p := range r.PMT {
		if p.VersionChanges > 0 || p.Stale {
			return true
		}
	}
	for _, s := range r.Streams {
		if s.DuplicatePackets > 0 || s.LostPackets > 0 {
			return true
		}
	}
	return false
}

func hasAnomalies(a demux.Anomalies) bool {
	return a.NonMultipleOf188 || a.SyncLossEvents > 0 || a.ZeroFillSegments > 0 || a.TrailingBytes > 0 ||
		a.CRCErrors > 0 || a.IncompleteSections > 0 || a.TransportErrors > 0 || a.PacketParseErrors > 0
}

// PrintScan writes a human-readable scan summary.
func PrintScan(w io.Writer, r Report) {
	fmt.Fprintf(w, "file: %s\npackets: %d  size: %d bytes  duration: %d ms\n", r.File, r.PacketsTotal, r.SizeBytes, r.DurationMS)
	fmt.Fprintf(w, "PAT: v%s..v%s changes=%d stale=%t\n", version(r.PAT.FirstVersion), version(r.PAT.LastVersion), r.PAT.VersionChanges, r.PAT.Stale)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PMT_PID\tPROGRAM\tVERSION\tCHANGES\tSTALE\tSTREAM_COUNT")
	for _, p := range r.PMT {
		fmt.Fprintf(tw, "%d\t%d\t%s..%s\t%d\t%t\t%d\n", p.PID, p.ProgramNumber, version(p.FirstVersion), version(p.LastVersion), p.VersionChanges, p.Stale, streamCount(p))
	}
	fmt.Fprintln(tw, "PID\tPROGRAM\tTYPE\tPACKETS\tPAYLOAD\tDUP\tLOST")
	for _, s := range r.Streams {
		fmt.Fprintf(tw, "%d\t%d\t0x%02x\t%d\t%d\t%d\t%d\n", s.PID, s.ProgramNumber, s.StreamType, s.PacketCount, s.PayloadBytes, s.DuplicatePackets, s.LostPackets)
	}
	fmt.Fprintf(tw, "8191\t-\tnull\t%d\t%d\t0\t0\n", r.NullPID.PacketCount, r.NullPID.PayloadBytes)
	_ = tw.Flush()
	if len(r.CCGaps) > 0 {
		fmt.Fprintln(w, "cc gaps:")
		for _, g := range r.CCGaps {
			fmt.Fprintf(w, "  pid=%d packets=%d..%d expected_cc=%d actual_cc=%d missing=%d\n", g.PID, g.StartPacket, g.EndPacket, g.ExpectedCC, g.ActualCC, g.MissingPackets)
		}
	}
	a := r.Anomalies
	fmt.Fprintf(w, "anomalies: lost=%d non_multiple=%t crc=%d sync_loss=%d/%dB zero_fill=%d/%dB trailing=%dB incomplete_sections=%d ignored_next=%d transport=%d parse=%d unknown_tables=%d\n",
		r.LostPackets, a.NonMultipleOf188, a.CRCErrors, a.SyncLossEvents, a.SyncLossBytes, a.ZeroFillSegments, a.ZeroFillBytes,
		a.TrailingBytes, a.IncompleteSections, a.IgnoredNextTables, a.TransportErrors, a.PacketParseErrors, a.UnknownSections)
}

func streamCount(t TableReport) int {
	for _, p := range t.Mapping.Programs {
		if p.ProgramNumber == t.ProgramNumber {
			return len(p.Streams)
		}
	}
	return 0
}

func version(v *uint8) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *v)
}
