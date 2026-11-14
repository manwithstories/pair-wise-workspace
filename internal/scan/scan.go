// Package scan orchestrates one streaming pass over a transport stream.
package scan

import (
	"errors"
	"io"
	"os"

	"tsaudit/internal/counter"
	"tsaudit/internal/psi"
	"tsaudit/internal/report"
	"tsaudit/internal/segment"
	"tsaudit/internal/tspipe"
)

type Options struct {
	Input           string
	FileSize        int64
	PMTTimeout      int64
	MinSegmentBytes int64
}

func File(path string, opts Options) (*report.Report, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opts.Input = path
	opts.FileSize = info.Size()
	return Stream(f, opts)
}

func Stream(r io.Reader, opts Options) (*report.Report, error) {
	if opts.MinSegmentBytes < 0 {
		return nil, errors.New("scan: min segment bytes cannot be negative")
	}
	if opts.MinSegmentBytes == 0 {
		opts.MinSegmentBytes = 64 * 1024
	}
	reader := tspipe.NewReader(r)
	assembler := psi.NewAssembler()
	registry := psi.NewRegistry(opts.PMTTimeout)
	tracker := counter.NewTracker()
	pidCounts := make(map[uint16]int64)
	boundaries := make([]segment.Boundary, 0)
	initialSnapshotID := 0
	lastSnapshotID := 0
	seenNonInitial := false
	previousChanges := 0
	transportErrors := int64(0)
	packetIndex := int64(0)

	for {
		p, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		packetIndex++
		pidCounts[p.PID]++
		registry.Tick(packetIndex)
		if p.TEI {
			transportErrors++
		} else if p.HasPayload {
			if p.PID == 0 {
				for _, sec := range assembler.Feed(p) {
					registry.HandleSection(sec)
				}
			} else if registry.IsPMTPID(p.PID) {
				for _, sec := range assembler.Feed(p) {
					registry.HandleSection(sec)
				}
			}
		}
		if jump := tracker.Observe(p); jump != nil {
			jump.SnapshotID = registry.CurrentSnapshot().ID
			boundaries = append(boundaries, segment.Boundary{
				Offset: jump.Offset, Kind: "cc_break", SnapshotID: jump.SnapshotID,
			})
		}
		changes := registry.Changes
		for ; previousChanges < len(changes); previousChanges++ {
			change := changes[previousChanges]
			if change.Initial {
				lastSnapshotID = change.SnapshotID
				continue
			}
			if !seenNonInitial {
				initialSnapshotID = lastSnapshotID
				seenNonInitial = true
			}
			boundaries = append(boundaries, segment.Boundary{
				Offset: change.Offset, Kind: "version", SnapshotID: change.SnapshotID,
			})
			lastSnapshotID = change.SnapshotID
		}
	}
	registry.Finalize()
	fileSize := opts.FileSize
	if fileSize <= 0 {
		fileSize = reader.EndOffset()
	}
	segments := segment.Plan(boundaries, fileSize, opts.MinSegmentBytes, initialSnapshotID)
	pidMap := append([]psi.PIDEntry(nil), registry.CurrentSnapshot().Entries...)
	esTotal := int64(0)
	counted := make(map[uint16]bool)
	for i := range pidMap {
		pidMap[i].PacketCount = pidCounts[pidMap[i].PID]
		if pidMap[i].Role == "ES" && !counted[pidMap[i].PID] {
			esTotal += pidMap[i].PacketCount
			counted[pidMap[i].PID] = true
		}
	}
	if pidMap == nil {
		pidMap = []psi.PIDEntry{}
	}
	conflicts := registry.Conflicts
	if conflicts == nil {
		conflicts = []psi.Conflict{}
	}
	warnings := registry.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	out := &report.Report{
		Input: opts.Input, Packets: reader.Packets(), Bytes: fileSize,
		SyncErrors: reader.SyncErrors(), ResyncLostBytes: reader.LostBytes(),
		TransportErrors: transportErrors, CRCFailures: assembler.CRCErrors,
		DuplicateSections: assembler.DuplicateSections,
		PATVersionChanges: registry.PATVersionChanges(), PMTVersionChanges: registry.PMTVersionChanges(),
		PIDMap: pidMap, ESPacketsTotal: esTotal,
		CCJumps: len(tracker.Jumps), CCJumpEvents: tracker.Jumps,
		CCDuplicates: tracker.Duplicates, LostPackets: tracker.Lost,
		Segments: len(segments), SliceManifest: segments,
		VersionChanges: registry.Changes, Conflicts: conflicts, Warnings: warnings,
		Success: len(conflicts) == 0 && assembler.CRCErrors == 0 && transportErrors == 0 &&
			reader.SyncErrors() == 0 && len(warnings) == 0,
	}
	if out.CCJumpEvents == nil {
		out.CCJumpEvents = []counter.Jump{}
	}
	if out.VersionChanges == nil {
		out.VersionChanges = []psi.Change{}
	}
	return out, nil
}
