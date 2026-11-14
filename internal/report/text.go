package report

import (
	"fmt"
	"io"

	"tsprobe/internal/psi"
)

// WriteText prints a human-readable summary.
func (r *Report) WriteText(w io.Writer) {
	fmt.Fprintf(w, "file=%s packet_size=%d packets=%d bytes=%d\n",
		r.File, r.PacketSize, r.Packets, r.Bytes)
	fmt.Fprintf(w, "bad_sync=%d malformed=%d crc_errors=%d length_errors=%d\n",
		r.BadSync, r.Malformed, r.CRCErrors, r.LengthErrors)
	fmt.Fprintf(w, "pat_found=%v pat_version=%d pat_version_changes=%d "+
		"(repeats=%d skips=%d rollbacks=%d)\n",
		r.PATFound, r.PATVersion, r.PATVersionChanges,
		r.PATVersionStats.Repeats, r.PATVersionStats.Skips, r.PATVersionStats.Rollbacks)
	for _, p := range r.Programs {
		fmt.Fprintf(w, "program=%d pmt_pid=0x%04X pcr_pid=0x%04X pmt_version=%d "+
			"pmt_version_changes=%d (repeats=%d skips=%d rollbacks=%d)\n",
			p.ProgramNumber, p.PMTPID, p.PCRPID, p.PMTVersion,
			p.Versions.Changes, p.Versions.Repeats, p.Versions.Skips, p.Versions.Rollbacks)
		for _, s := range p.Streams {
			fmt.Fprintf(w, "  es pid=0x%04X stream_type=0x%02X %s\n",
				s.PID, s.StreamType, psi.StreamTypeName(s.StreamType))
		}
	}
	fmt.Fprintf(w, "cc_jump_events=%d loss_events=%d lost_packets_est=%d duplicates=%d\n",
		r.CCJumpEvents, r.LossEvents, r.LostPacketsEst, r.Duplicates)
	fmt.Fprintf(w, "null_packets=%d es_packets=%d psi_packets=%d\n",
		r.NullPackets, r.ESPackets, r.PSIPackets)
	fmt.Fprintf(w, "PID table:\n")
	for _, p := range r.PIDs {
		fmt.Fprintf(w, "  pid=0x%04X role=%s program=%d %s packets=%d bytes=%d\n",
			p.PID, p.Role, p.Program, p.StreamType, p.Packets, p.Bytes)
	}
}
