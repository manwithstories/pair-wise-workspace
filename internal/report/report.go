package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"tsprobe/internal/es"
	"tsprobe/internal/psi"
	"tsprobe/internal/slicer"
	"tsprobe/internal/tspkt"
)

type PATProgram struct {
	ProgramNumber uint16 `json:"program_number"`
	PMTPID        uint16 `json:"pmt_pid"`
}

type PATRevision struct {
	Revision          int          `json:"revision"`
	Version           int          `json:"version"`
	EffectivePacket   int64        `json:"effective_packet"`
	TransportStreamID uint16       `json:"transport_stream_id"`
	Programs          []PATProgram `json:"programs"`
}

type PATReport struct {
	Revisions      int           `json:"revisions"`
	CurrentVersion int           `json:"current_version"`
	History        []PATRevision `json:"history"`
}

type StreamReport struct {
	PID        uint16 `json:"pid"`
	StreamType int    `json:"stream_type"`
}

type PMTRevision struct {
	Revision        int            `json:"revision"`
	Version         int            `json:"version"`
	EffectivePacket int64          `json:"effective_packet"`
	PCRPID          uint16         `json:"pcr_pid"`
	Streams         []StreamReport `json:"streams"`
}

type ESVersion struct {
	PMTRevision     int   `json:"pmt_revision"`
	PMTVersion      int   `json:"pmt_version"`
	EffectivePacket int64 `json:"effective_packet"`
	StreamType      int   `json:"stream_type"`
	Packets         int64 `json:"packets"`
	PayloadBytes    int64 `json:"payload_bytes"`
	PESStarts       int64 `json:"pes_starts"`
	PCRPackets      int64 `json:"pcr_packets"`
	CCPackets       int64 `json:"cc_packets"`
	CCAhead         int64 `json:"cc_ahead"`
	CCBackwards     int64 `json:"cc_backwards"`
	Duplicates      int64 `json:"duplicates"`
	Discontinuities int64 `json:"discontinuities"`
	PESTruncated    int64 `json:"pes_truncated"`
	LastCC          int   `json:"last_cc"`
}

type ESReport struct {
	PID             uint16      `json:"pid"`
	StreamType      int         `json:"stream_type"`
	Packets         int64       `json:"packets"`
	PayloadBytes    int64       `json:"payload_bytes"`
	PESStarts       int64       `json:"pes_starts"`
	PCRPackets      int64       `json:"pcr_packets"`
	CCPackets       int64       `json:"cc_packets"`
	CCAhead         int64       `json:"cc_ahead"`
	CCBackwards     int64       `json:"cc_backwards"`
	Duplicates      int64       `json:"duplicates"`
	Discontinuities int64       `json:"discontinuities"`
	PESTruncated    int64       `json:"pes_truncated"`
	LastCC          int         `json:"last_cc"`
	HasPTS          bool        `json:"has_pts"`
	FirstPTS        int64       `json:"first_pts"`
	LastPTS         int64       `json:"last_pts"`
	HasDTS          bool        `json:"has_dts"`
	FirstDTS        int64       `json:"first_dts"`
	LastDTS         int64       `json:"last_dts"`
	Versions        []ESVersion `json:"versions"`
}

type ProgramReport struct {
	ProgramNumber  int           `json:"program_number"`
	PMTPID         uint16        `json:"pmt_pid"`
	PMTRevisions   int           `json:"pmt_revisions"`
	CurrentVersion int           `json:"current_version"`
	ES             []ESReport    `json:"es"`
	PMTHistory     []PMTRevision `json:"pmt_history"`
}

type Report struct {
	Status            string           `json:"status"`
	Input             string           `json:"input_file"`
	PacketSize        int              `json:"packet_size"`
	PacketsTotal      int64            `json:"packets_total"`
	PayloadBytes      int64            `json:"payload_bytes"`
	ResyncEvents      int64            `json:"resync_events"`
	TruncatedPackets  int64            `json:"truncated_packets"`
	TruncatedBytes    int64            `json:"truncated_bytes"`
	Sections          int64            `json:"sections"`
	CRCErrors         int64            `json:"crc_errors"`
	TruncatedSections int64            `json:"truncated_sections"`
	PESTruncated      int64            `json:"pes_truncated"`
	NullPackets       int64            `json:"null_packets"`
	PSIPackets        int64            `json:"psi_packets"`
	OtherPackets      int64            `json:"other_packets"`
	ESPackets         int64            `json:"es_packets"`
	CCAhead           int64            `json:"cc_ahead"`
	CCBackwards       int64            `json:"cc_backwards"`
	Duplicates        int64            `json:"duplicates"`
	Discontinuities   int64            `json:"discontinuities"`
	PAT               PATReport        `json:"pat"`
	Programs          []ProgramReport  `json:"programs"`
	Segments          []slicer.Segment `json:"segments"`
}

type BuildInput struct {
	Status   string
	Input    string
	Reader   tspkt.Stats
	Sections psi.SectionStats
	Registry *psi.Registry
	Counter  *es.Counter
	Segments []slicer.Segment
}

func Build(in BuildInput) Report {
	totals := in.Counter.Totals()
	out := Report{
		Status: in.Status, Input: in.Input, PacketSize: in.Reader.PacketSize,
		PacketsTotal: in.Reader.Packets, PayloadBytes: totals.PayloadBytes,
		ResyncEvents: in.Reader.ResyncEvents, TruncatedPackets: in.Reader.TruncatedPackets,
		TruncatedBytes: in.Reader.TruncatedBytes, Sections: in.Sections.Sections,
		CRCErrors: in.Sections.CRCErrors, TruncatedSections: in.Sections.TruncatedSections,
		PESTruncated: totals.PESTruncated, NullPackets: totals.NullPackets,
		PSIPackets: totals.PSIPackets, OtherPackets: totals.OtherPackets,
		ESPackets: totals.ESPackets, CCAhead: totals.CCAhead,
		CCBackwards: totals.CCBackwards, Duplicates: totals.Duplicates,
		Discontinuities: totals.Discontinuities, Programs: []ProgramReport{},
		Segments: append([]slicer.Segment{}, in.Segments...),
	}
	out.PAT = buildPAT(in.Registry)
	buckets := in.Counter.Buckets()
	for _, program := range in.Registry.CurrentProgramNumbers() {
		out.Programs = append(out.Programs, buildProgram(program, in.Registry, buckets))
	}
	return out
}

func buildPAT(reg *psi.Registry) PATReport {
	history := reg.PATHistory()
	out := PATReport{Revisions: len(history), History: []PATRevision{}}
	if current := reg.PAT(); current != nil {
		out.CurrentVersion = int(current.Version)
	}
	for _, snapshot := range history {
		revision := PATRevision{
			Revision: snapshot.Revision, Version: int(snapshot.Version),
			EffectivePacket: snapshot.EffectivePacket, TransportStreamID: snapshot.TransportStreamID,
			Programs: []PATProgram{},
		}
		for _, entry := range snapshot.Programs {
			revision.Programs = append(revision.Programs, PATProgram{ProgramNumber: entry.ProgramNumber, PMTPID: entry.PMTPID})
		}
		out.History = append(out.History, revision)
	}
	return out
}

func buildProgram(program uint16, reg *psi.Registry, buckets []es.Bucket) ProgramReport {
	pmtPID, _ := reg.CurrentPMTPID(program)
	history := reg.PMTHistory(program)
	out := ProgramReport{
		ProgramNumber: int(program), PMTPID: pmtPID, PMTRevisions: len(history),
		ES: []ESReport{}, PMTHistory: []PMTRevision{},
	}
	if len(history) > 0 {
		out.CurrentVersion = int(history[len(history)-1].Version)
	}
	streamTypes := make(map[uint16]uint8)
	for _, snapshot := range history {
		revision := PMTRevision{
			Revision: snapshot.Revision, Version: int(snapshot.Version),
			EffectivePacket: snapshot.EffectivePacket, PCRPID: snapshot.PCRPID,
			Streams: []StreamReport{},
		}
		for _, stream := range snapshot.Streams {
			streamTypes[stream.PID] = stream.StreamType
			revision.Streams = append(revision.Streams, StreamReport{PID: stream.PID, StreamType: int(stream.StreamType)})
		}
		out.PMTHistory = append(out.PMTHistory, revision)
	}
	pids := make([]int, 0, len(streamTypes))
	for pid := range streamTypes {
		pids = append(pids, int(pid))
	}
	sort.Ints(pids)
	for _, rawPID := range pids {
		pid := uint16(rawPID)
		report := ESReport{PID: pid, StreamType: int(streamTypes[pid]), Versions: []ESVersion{}}
		for _, bucket := range buckets {
			if bucket.ProgramNumber != program || bucket.PID != pid {
				continue
			}
			aggregateES(&report, bucket)
		}
		out.ES = append(out.ES, report)
	}
	return out
}

func aggregateES(report *ESReport, bucket es.Bucket) {
	report.Packets += bucket.Packets
	report.PayloadBytes += bucket.PayloadBytes
	report.PESStarts += bucket.PESStarts
	report.PCRPackets += bucket.PCRPackets
	report.CCPackets += bucket.CCPackets
	report.CCAhead += bucket.CCAhead
	report.CCBackwards += bucket.CCBackwards
	report.Duplicates += bucket.Duplicates
	report.Discontinuities += bucket.Discontinuities
	report.PESTruncated += bucket.PESTruncated
	report.LastCC = bucket.LastCC
	if bucket.HasPTS {
		if !report.HasPTS {
			report.FirstPTS = bucket.FirstPTS
		}
		report.HasPTS = true
		report.LastPTS = bucket.LastPTS
	}
	if bucket.HasDTS {
		if !report.HasDTS {
			report.FirstDTS = bucket.FirstDTS
		}
		report.HasDTS = true
		report.LastDTS = bucket.LastDTS
	}
	report.Versions = append(report.Versions, ESVersion{
		PMTRevision: bucket.PMTRevision, PMTVersion: int(bucket.PMTVersion),
		EffectivePacket: bucket.Effective, StreamType: int(bucket.StreamType),
		Packets: bucket.Packets, PayloadBytes: bucket.PayloadBytes, PESStarts: bucket.PESStarts,
		PCRPackets: bucket.PCRPackets, CCPackets: bucket.CCPackets, CCAhead: bucket.CCAhead,
		CCBackwards: bucket.CCBackwards, Duplicates: bucket.Duplicates,
		Discontinuities: bucket.Discontinuities, PESTruncated: bucket.PESTruncated, LastCC: bucket.LastCC,
	})
}

func WriteJSON(w io.Writer, r Report) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err = io.WriteString(w, "\n")
	return err
}

func WriteTable(w io.Writer, r Report) error {
	if _, err := fmt.Fprintf(w, "status=%s input=%s\n", r.Status, r.Input); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "packet_size=%d packets_total=%d payload_bytes=%d\n", r.PacketSize, r.PacketsTotal, r.PayloadBytes); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "psi_packets=%d null_packets=%d other_packets=%d es_packets=%d\n", r.PSIPackets, r.NullPackets, r.OtherPackets, r.ESPackets); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "cc_ahead=%d cc_backwards=%d duplicates=%d discontinuities=%d crc_errors=%d\n", r.CCAhead, r.CCBackwards, r.Duplicates, r.Discontinuities, r.CRCErrors); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "PAT revisions=%d current_version=%d\n", r.PAT.Revisions, r.PAT.CurrentVersion); err != nil {
		return err
	}
	for _, program := range r.Programs {
		if _, err := fmt.Fprintf(w, "program=%d pmt_pid=%d pmt_revisions=%d current_version=%d\n", program.ProgramNumber, program.PMTPID, program.PMTRevisions, program.CurrentVersion); err != nil {
			return err
		}
		for _, stream := range program.ES {
			if _, err := fmt.Fprintf(w, "  es pid=%d stream_type=%#x packets=%d pes=%d pcr=%d cc_ahead=%d cc_backwards=%d duplicates=%d\n", stream.PID, stream.StreamType, stream.Packets, stream.PESStarts, stream.PCRPackets, stream.CCAhead, stream.CCBackwards, stream.Duplicates); err != nil {
				return err
			}
		}
	}
	for _, segment := range r.Segments {
		if _, err := fmt.Fprintf(w, "segment=%d start_packet=%d end_packet=%d duration_ms=%d psi_revision=%d switches=%d\n", segment.Index, segment.StartPacket, segment.EndPacket, segment.DurationMS, segment.PSIRevision, len(segment.PSISwitches)); err != nil {
			return err
		}
	}
	return nil
}
