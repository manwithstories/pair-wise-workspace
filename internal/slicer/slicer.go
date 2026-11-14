// Package slicer cuts a TS stream into byte-sized segments, re-injecting
// the current PAT/PMT at the head of every segment.
package slicer

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"tsprobe/internal/psi"
	"tsprobe/internal/tspkt"
)

// SegmentInfo describes one written segment.
type SegmentInfo struct {
	Index       int               `json:"index"`
	File        string            `json:"file"`
	StartPacket uint64            `json:"start_packet"` // first source packet index in segment
	EndPacket   uint64            `json:"end_packet"`   // one past the last source packet index
	Bytes       uint64            `json:"bytes"`
	PIDCounts   map[uint16]uint64 `json:"pid_counts"`
}

// Manifest is the slicer output descriptor.
type Manifest struct {
	Input    string        `json:"input"`
	GopBytes uint64        `json:"gop_bytes"`
	Segments []SegmentInfo `json:"segments"`
}

// Slicer cuts streams into segments.
type Slicer struct {
	reg *psi.Registry
}

// New creates a Slicer.
func New() *Slicer { return &Slicer{reg: psi.NewRegistry()} }

// CleanDir removes stale segment files and the manifest from outDir.
func CleanDir(outDir string) error {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "segment_") && strings.HasSuffix(e.Name(), ".ts") ||
			e.Name() == "manifest.json" {
			if err := os.Remove(filepath.Join(outDir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// buildPSIPacket wraps one section into a single 188-byte packet.
func buildPSIPacket(pid uint16, ccVal byte, section []byte) []byte {
	pkt := make([]byte, tspkt.PacketSize)
	pkt[0] = tspkt.SyncByte
	pkt[1] = 0x40 | byte(pid>>8)&0x1F // PUSI set
	pkt[2] = byte(pid)
	pkt[3] = 0x10 | (ccVal & 0x0F) // payload only
	pkt[4] = 0x00                  // pointer_field
	n := copy(pkt[5:], section)
	for i := 5 + n; i < len(pkt); i++ {
		pkt[i] = 0xFF
	}
	return pkt
}

// psiPacket pairs a PID with its raw section for injection.
type psiPacket struct {
	pid     uint16
	section []byte
}

// currentPSI returns PAT then PMT packets of the current versions.
func (s *Slicer) currentPSI() []psiPacket {
	var out []psiPacket
	for _, sec := range s.reg.PATRawSections() {
		out = append(out, psiPacket{pid: 0, section: sec})
	}
	if s.reg.PAT == nil {
		return out
	}
	progs := s.reg.Programs()
	raw := s.reg.PMTRawSections()
	for i, p := range progs {
		if i < len(raw) {
			out = append(out, psiPacket{pid: p.PMTPID, section: raw[i]})
		}
	}
	return out
}

// Slice cuts src into segments under outDir and writes the manifest.
func (s *Slicer) Slice(src io.Reader, inputName, outDir, manifestPath string, gopBytes uint64) (*Manifest, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	if err := CleanDir(outDir); err != nil {
		return nil, err
	}
	rd, err := tspkt.NewReader(src)
	if err != nil {
		return nil, err
	}
	var psiCnt psi.Counters
	assemblers := map[uint16]*psi.Assembler{}
	getAsm := func(pid uint16) *psi.Assembler {
		a := assemblers[pid]
		if a == nil {
			a = psi.NewAssembler(pid)
			assemblers[pid] = a
		}
		return a
	}
	m := &Manifest{Input: inputName, GopBytes: gopBytes}
	var (
		out      *os.File
		seg      *SegmentInfo
		segBytes uint64
		pktIdx   uint64
	)
	closeSeg := func() error {
		if out == nil {
			return nil
		}
		if err := out.Close(); err != nil {
			return err
		}
		seg.EndPacket = pktIdx
		seg.Bytes = segBytes
		m.Segments = append(m.Segments, *seg)
		out, seg = nil, nil
		return nil
	}
	openSeg := func() error {
		seg = &SegmentInfo{
			Index:       len(m.Segments),
			File:        fmt.Sprintf("segment_%05d.ts", len(m.Segments)),
			StartPacket: pktIdx,
			PIDCounts:   map[uint16]uint64{},
		}
		f, err := os.Create(filepath.Join(outDir, seg.File))
		if err != nil {
			return err
		}
		out = f
		segBytes = 0
		// Re-inject current PAT/PMT with CC renumbered from zero.
		ccPerPID := map[uint16]byte{}
		for _, pp := range s.currentPSI() {
			pkt := buildPSIPacket(pp.pid, ccPerPID[pp.pid], pp.section)
			ccPerPID[pp.pid]++
			if _, err := out.Write(pkt); err != nil {
				return err
			}
			seg.PIDCounts[pp.pid]++
			segBytes += tspkt.PacketSize
		}
		return nil
	}
	for {
		pkt, err := rd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		isPSI := pkt.PID == 0 || s.reg.IsPMTPID(pkt.PID)
		if pkt.HasPayload && isPSI {
			secs := getAsm(pkt.PID).Feed(pkt.Payload, pkt.PayloadUnitStart, pkt.Offset, &psiCnt)
			for _, sec := range secs {
				if pkt.PID == 0 {
					s.reg.ApplyPATSection(sec.Raw)
				} else {
					s.reg.ApplyPMTSection(sec.Raw)
				}
			}
		}
		if isPSI {
			// Original PSI is replaced by re-injection at segment heads.
			pktIdx++
			continue
		}
		if out == nil || segBytes >= gopBytes {
			if err := closeSeg(); err != nil {
				return nil, err
			}
			if err := openSeg(); err != nil {
				return nil, err
			}
		}
		if _, err := out.Write(pkt.Raw); err != nil {
			return nil, err
		}
		seg.PIDCounts[pkt.PID]++
		segBytes += tspkt.PacketSize
		pktIdx++
	}
	if err := closeSeg(); err != nil {
		return nil, err
	}
	if manifestPath != "" {
		if err := m.Write(manifestPath); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Write saves the manifest as indented JSON.
func (m *Manifest) Write(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

// SortedPIDs returns manifest PID keys in order (helper for verify).
func (s *SegmentInfo) SortedPIDs() []int {
	pids := make([]int, 0, len(s.PIDCounts))
	for pid := range s.PIDCounts {
		pids = append(pids, int(pid))
	}
	sort.Ints(pids)
	return pids
}
