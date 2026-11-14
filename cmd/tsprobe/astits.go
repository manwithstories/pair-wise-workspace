package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/asticode/go-astits"

	"tsprobe/internal/report"
)

// astitsCheck independently re-parses the stream with go-astits and
// compares the PID set and per-PID packet counts against the report.
// It returns "" on agreement, otherwise a description of the differences.
func astitsCheck(path string, rep *report.Report) string {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("open: %v", err)
	}
	defer f.Close()
	demuxer := astits.NewDemuxer(context.Background(), f)
	counts := map[uint16]uint64{}
	for {
		pkt, err := demuxer.NextPacket()
		if errors.Is(err, astits.ErrNoMorePackets) {
			break
		}
		if err != nil {
			return fmt.Sprintf("next packet: %v", err)
		}
		counts[pkt.Header.PID]++
	}
	mine := pidCounts(rep)
	var diffs []string
	pids := map[uint16]bool{}
	for pid := range counts {
		pids[pid] = true
	}
	for pid := range mine {
		pids[pid] = true
	}
	sorted := make([]int, 0, len(pids))
	for pid := range pids {
		sorted = append(sorted, int(pid))
	}
	sort.Ints(sorted)
	for _, p := range sorted {
		pid := uint16(p)
		if counts[pid] != mine[pid] {
			diffs = append(diffs, fmt.Sprintf("pid 0x%04X tsprobe=%d astits=%d",
				pid, mine[pid], counts[pid]))
		}
	}
	return strings.Join(diffs, "; ")
}
