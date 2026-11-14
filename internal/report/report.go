package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"tsprobe/internal/demux"
	"tsprobe/internal/psi"
	"tsprobe/internal/slicer"
	"tsprobe/internal/tspacket"
)

// Summary is the stable JSON contract used by scan, slice and verify.
type Summary struct {
	PATVersions  int                   `json:"pat_versions"`
	PMTVersions  []psi.ProgramVersions `json:"pmt_versions"`
	PIDMap       []psi.PIDInterval     `json:"pid_map"`
	PacketCounts map[uint16]int64      `json:"packet_counts"`
	CCJumps      int64                 `json:"cc_jumps"`
	LostPackets  int64                 `json:"lost_packets"`
	CRCErrors    int64                 `json:"crc_errors"`
	Slices       []slicer.SliceInfo    `json:"slices"`
	Mismatches   int                   `json:"mismatches"`
}

func Build(store *psi.Store, packetStats tspacket.Stats, demuxStats demux.Stats,
	slices []slicer.SliceInfo, mismatches int) Summary {
	if slices == nil {
		slices = []slicer.SliceInfo{}
	}
	pmtVersions := store.PMTVersions()
	if pmtVersions == nil {
		pmtVersions = []psi.ProgramVersions{}
	}
	pidMap := store.PIDMap()
	if pidMap == nil {
		pidMap = []psi.PIDInterval{}
	}
	return Summary{
		PATVersions:  store.PATVersions(),
		PMTVersions:  pmtVersions,
		PIDMap:       pidMap,
		PacketCounts: store.PacketCounts(),
		CCJumps:      packetStats.CCJumps,
		LostPackets:  packetStats.LostPackets,
		CRCErrors:    demuxStats.CRCFailures,
		Slices:       slices,
		Mismatches:   mismatches,
	}
}

func (s Summary) WriteJSON(w io.Writer) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err = io.WriteString(w, "\n")
	return err
}

// WriteText prints a terminal-oriented summary with diagnostics that are not
// part of the fixed JSON schema.
func (s Summary) WriteText(w io.Writer, store *psi.Store, packetStats tspacket.Stats) error {
	if _, err := fmt.Fprintf(w, "PAT versions: %d\n", s.PATVersions); err != nil {
		return err
	}
	for _, item := range s.PMTVersions {
		if _, err := fmt.Fprintf(w, "PMT program %d versions: %d\n", item.Program, item.Versions); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "PID map intervals: %d\n", len(s.PIDMap)); err != nil {
		return err
	}
	for _, item := range s.PIDMap {
		if _, err := fmt.Fprintf(w, "  PID %d program %d %s versions %d-%d packets %d\n",
			item.PID, item.Program, item.StreamType, item.StartVersion, item.EndVersion, item.PacketCount); err != nil {
			return err
		}
	}
	pids := make([]int, 0, len(s.PacketCounts))
	for pid := range s.PacketCounts {
		pids = append(pids, int(pid))
	}
	sort.Ints(pids)
	for _, pid := range pids {
		if _, err := fmt.Fprintf(w, "PID %d packets: %d\n", pid, s.PacketCounts[uint16(pid)]); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "CC jumps: %d\nLost packets: %d\nCRC errors: %d\n",
		s.CCJumps, s.LostPackets, s.CRCErrors); err != nil {
		return err
	}
	if store != nil {
		if dangling := store.DanglingPrograms(); len(dangling) > 0 {
			if _, err := fmt.Fprintf(w, "Dangling programs: %v\n", dangling); err != nil {
				return err
			}
		}
		if store.PATRollbacks() > 0 {
			if _, err := fmt.Fprintf(w, "PAT version rollbacks: %d\n", store.PATRollbacks()); err != nil {
				return err
			}
		}
		for _, program := range store.ProgramIntervals() {
			if _, err := fmt.Fprintf(w, "Program %d PMT PID %d versions %d-%d\n",
				program.Program, program.PMTPID, program.StartPATVersion, program.EndPATVersion); err != nil {
				return err
			}
		}
		rollbacks := store.PMTRollbacks()
		for program, count := range rollbacks {
			if count > 0 {
				if _, err := fmt.Fprintf(w, "PMT program %d version rollbacks: %d\n", program, count); err != nil {
					return err
				}
			}
		}
	}
	if _, err := fmt.Fprintf(w, "TEI: %d\nAdaptation-only: %d\nNull: %d\nDuplicates: %d\nSync resyncs: %d\n",
		packetStats.TEIPackets, packetStats.AdaptationOnly, packetStats.NullPackets,
		packetStats.DuplicatePackets, packetStats.SyncResyncs); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Slices: %d\nMismatches: %d\n", len(s.Slices), s.Mismatches); err != nil {
		return err
	}
	return nil
}
