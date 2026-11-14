package slicer

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"tsaudit/internal/ts"
)

func writeSegments(cfg Config, fp *firstPass) (*Index, error) {
	in, err := os.Open(cfg.In)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	rd, err := ts.NewReader(in)
	if err != nil {
		return nil, err
	}
	idx := &Index{Input: cfg.In, Program: cfg.Program, SegmentMS: cfg.SegmentMS}
	planIdx := 0
	var f *os.File
	var w *ts.Writer
	var current Segment
	var got []uint16
	closeSegment := func() error {
		if f == nil {
			return nil
		}
		if err := f.Close(); err != nil {
			return err
		}
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		current.PIDCount, current.PIDs = len(got), append([]uint16(nil), got...)
		idx.Segments = append(idx.Segments, current)
		f, w, got = nil, nil, nil
		return nil
	}
	for {
		p, rerr := rd.Next()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
		for planIdx < len(fp.plans) && p.Index > fp.plans[planIdx].end {
			if err := closeSegment(); err != nil {
				return nil, err
			}
			planIdx++
		}
		if planIdx >= len(fp.plans) {
			break
		}
		pl := fp.plans[planIdx]
		if p.Index < pl.start {
			continue
		}
		if f == nil {
			name := fmt.Sprintf("seg-%03d.ts", planIdx)
			f, err = os.Create(filepath.Join(cfg.OutDir, name))
			if err != nil {
				return nil, err
			}
			w = ts.NewWriter(f)
			ccPAT, ccPMT := uint8(0), uint8(0)
			if err := writeSection(w, 0, pl.patRaw, &ccPAT); err != nil {
				return nil, err
			}
			if err := writeSection(w, pl.pmtPID, pl.pmtRaw, &ccPMT); err != nil {
				return nil, err
			}
			current = Segment{File: name, StartPacket: pl.start, EndPacket: pl.end,
				PCRStart: pl.pcr, PATVersion: pl.patVer, PMTVersion: pl.pmtVer}
		}
		if pl.dataPIDs[p.Header.PID] {
			if err := w.WritePacket(p); err != nil {
				return nil, err
			}
			got = appendUnique(got, p.Header.PID)
		}
	}
	if err := closeSegment(); err != nil {
		return nil, err
	}
	return idx, nil
}

func writeSection(w *ts.Writer, pid uint16, section []byte, cc *uint8) error {
	if len(section) == 0 {
		return fmt.Errorf("slicer: empty PSI section for PID 0x%x", pid)
	}
	pos, first := 0, true
	for pos < len(section) {
		n := 184
		if first {
			n = 183
		}
		end := pos + n
		if end > len(section) {
			end = len(section)
		}
		payload := append([]byte(nil), section[pos:end]...)
		if first {
			payload = append([]byte{0}, payload...)
		}
		if err := w.Write(pid, *cc, first, payload, nil); err != nil {
			return err
		}
		*cc = (*cc + 1) & 0x0f
		first, pos = false, end
	}
	return nil
}

func clone(b []byte) []byte { return append([]byte(nil), b...) }

func clonePIDs(in map[uint16]bool) map[uint16]bool {
	out := make(map[uint16]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func appendUnique(dst []uint16, pid uint16) []uint16 {
	for _, p := range dst {
		if p == pid {
			return dst
		}
	}
	return append(dst, pid)
}
