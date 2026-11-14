package slicer

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"tsprobe/internal/demux"
	"tsprobe/internal/psi"
	"tsprobe/internal/tspacket"
)

// Verify re-reads every indexed clip and returns the mismatch count plus
// human-readable diagnostics. It uses the same PSI parser as scan and slice.
func Verify(indexPath, dir string) (int, []string, error) {
	index, err := ReadIndex(indexPath)
	if err != nil {
		return 0, nil, err
	}
	var details []string
	mismatches := 0
	for _, item := range index.Slices {
		path := filepath.Join(dir, item.File)
		info, statErr := os.Stat(path)
		count, snapshot, crcFailures, keyframes, verifyErr := verifyFile(path)
		if statErr != nil {
			mismatches++
			details = append(details, fmt.Sprintf("%s: %v", item.File, statErr))
			continue
		}
		if verifyErr != nil {
			mismatches++
			details = append(details, fmt.Sprintf("%s: %v", item.File, verifyErr))
			continue
		}
		if count != item.PacketCount {
			mismatches++
			details = append(details, fmt.Sprintf("%s: packet_count index=%d file=%d", item.File, item.PacketCount, count))
		}
		if info.Size() != item.ByteLength || item.ByteLength != int64(item.PacketCount)*188 {
			mismatches++
			details = append(details, fmt.Sprintf("%s: byte_length index=%d file=%d", item.File, item.ByteLength, info.Size()))
		}
		if crcFailures != 0 {
			mismatches++
			details = append(details, fmt.Sprintf("%s: crc_errors=%d", item.File, crcFailures))
		}
		if keyframes == 0 {
			mismatches++
			details = append(details, fmt.Sprintf("%s: no keyframe", item.File))
		}
		if !snapshotHasPAT(snapshot) {
			mismatches++
			details = append(details, fmt.Sprintf("%s: no valid PAT", item.File))
			continue
		}
		if snapshot.PATVersion != item.PATVersion {
			mismatches++
			details = append(details, fmt.Sprintf("%s: pat_version index=%d file=%d", item.File, item.PATVersion, snapshot.PATVersion))
		}
		filePMTs := snapshotPMTVersions(snapshot)
		if !samePMTEntries(filePMTs, item.PMTVersions) {
			mismatches++
			details = append(details, fmt.Sprintf("%s: pmt_versions index=%v file=%v", item.File, item.PMTVersions, filePMTs))
		}
		if len(filePMTs) == 0 {
			mismatches++
			details = append(details, fmt.Sprintf("%s: no valid PMT", item.File))
		} else if !consistentPMT(snapshot) {
			mismatches++
			details = append(details, fmt.Sprintf("%s: PAT/PMT inconsistent", item.File))
		}
	}
	return mismatches, details, nil
}

func verifyFile(path string) (int, psi.Snapshot, int64, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, psi.Snapshot{}, 0, 0, err
	}
	defer file.Close()
	reader := tspacket.NewReader(file, 8)
	store := psi.NewStore()
	dmx := demux.New(store)
	var packets int
	for {
		packet, err := reader.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, psi.Snapshot{}, 0, 0, err
		}
		packets++
		if _, err := dmx.Process(packet); err != nil {
			return 0, psi.Snapshot{}, 0, 0, err
		}
	}
	stats := dmx.Stats()
	return packets, store.Snapshot(), stats.CRCFailures, stats.Keyframes, nil
}

func snapshotHasPAT(snapshot psi.Snapshot) bool {
	return len(snapshot.Programs) > 0
}

func snapshotPMTVersions(snapshot psi.Snapshot) []PMTEntry {
	out := make([]PMTEntry, 0, len(snapshot.Programs))
	for _, program := range snapshot.Programs {
		if program.HasPMT {
			out = append(out, PMTEntry{Program: program.Program, Version: program.PMTVersion})
		}
	}
	return out
}

func samePMTEntries(a, b []PMTEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func consistentPMT(snapshot psi.Snapshot) bool {
	for _, program := range snapshot.Programs {
		if !program.HasPMT {
			continue
		}
		if program.PMT.Program != program.Program || program.PMT.PCRPID == 0x1fff {
			return false
		}
		pcrMapped := false
		for _, stream := range program.PMT.Streams {
			if stream.ElementaryPID == program.PMT.PCRPID {
				pcrMapped = true
			}
			if stream.ElementaryPID == 0x1fff {
				return false
			}
		}
		if !pcrMapped {
			return false
		}
	}
	return true
}
