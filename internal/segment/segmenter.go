// Package segment cuts a multiplex into per-program segments: by PCR
// time, and immediately on program table version changes. Each new
// segment starts with the current PAT/PMT rewritten at its head.
package segment

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"tsprobe/internal/program"
	"tsprobe/internal/psi"
	"tsprobe/internal/tsdemux"
)

// StreamInfo describes one elementary stream in a segment manifest.
type StreamInfo struct {
	PID          uint16 `json:"pid"`
	StreamType   uint8  `json:"stream_type"`
	Lang         string `json:"lang,omitempty"`
	Registration string `json:"registration,omitempty"`
}

// Info describes one written segment.
type Info struct {
	Index       int            `json:"index"`
	File        string         `json:"file"`
	StartMS     int64          `json:"start_ms"`
	EndMS       int64          `json:"end_ms"`
	PATVersion  int            `json:"pat_version"`
	PMTVersions map[string]int `json:"pmt_versions"`
	Streams     []StreamInfo   `json:"streams"`
	Packets     int64          `json:"packets"`
}

// Manifest is the seg command output manifest.
type Manifest struct {
	Input    string `json:"input"`
	Program  uint16 `json:"program"`
	MS       int64  `json:"ms"`
	Segments []Info `json:"segments"`
}

// Segmenter consumes demuxed packets after the program state has been
// updated for the same packet.
type Segmenter struct {
	dir      string
	ms       int64
	program  uint16
	st       *program.State
	f        *os.File
	info     *Info
	segments []Info
	include  map[uint16]bool
	pcrPID   uint16
	havePCR  bool
	segStart int64
	lastMS   int64
	revision int64
	wrotePMT bool
	content  int64 // non table packets in the open segment
}

func New(dir string, program uint16, ms int64, st *program.State) *Segmenter {
	return &Segmenter{
		dir: dir, ms: ms, program: program, st: st,
		include: map[uint16]bool{0: true},
	}
}

// refresh recomputes the included PID set from the current table view
// and, while a segment is open, refreshes its manifest snapshot.
func (s *Segmenter) refresh() {
	s.revision = s.st.Revision
	s.include = map[uint16]bool{0: true}
	if pmtPID, ok := s.st.PMTPID[s.program]; ok {
		s.include[pmtPID] = true
	}
	if pcr, ok := s.st.PCRPID[s.program]; ok {
		s.pcrPID = pcr
		s.include[pcr] = true
	}
	for _, st := range s.st.Streams[s.program] {
		s.include[st.PID] = true
	}
	if s.info != nil {
		s.snapshot()
		if !s.wrotePMT && len(s.st.CurrentPMT) > 0 {
			s.writePMTs()
			s.wrotePMT = true
		}
	}
}

// Observe processes one packet. tableChanged must be true when the
// packet completed a PAT/PMT version change in the program state.
func (s *Segmenter) Observe(p *tsdemux.Packet, tableChanged bool) error {
	if tableChanged {
		// Cut only once per table emission: consecutive version
		// events (e.g. PMTs of several programs in the same slot)
		// coalesce while no content packet was written yet.
		if s.info != nil && s.content > 0 {
			if err := s.cut(); err != nil {
				return err
			}
		}
		s.refresh()
		s.havePCR = false // re-anchor the time base on the next PCR
	} else if s.st.Revision != s.revision {
		s.refresh()
	}
	if s.info == nil {
		s.refresh()
		if err := s.open(); err != nil {
			return err
		}
	}
	if p.HasPCR && s.pcrPID != 0 && p.PID == s.pcrPID {
		ms := p.PCR90 / 90
		if !s.havePCR {
			s.havePCR, s.segStart = true, p.PCR90
			s.info.StartMS = ms
		} else if (p.PCR90-s.segStart)/90 >= s.ms && s.info.Packets > 0 {
			if err := s.cut(); err != nil {
				return err
			}
			if err := s.open(); err != nil {
				return err
			}
			s.segStart = p.PCR90
			s.info.StartMS = ms
		}
		s.lastMS = ms
	}
	if !s.include[p.PID] {
		return nil
	}
	if _, err := s.f.Write(packetBytes(p)); err != nil {
		return err
	}
	s.info.Packets++
	s.info.EndMS = s.lastMS
	if p.PID != 0 {
		if _, isPMT := s.st.PMTOwner[p.PID]; !isPMT {
			s.content++
		}
	}
	return nil
}

// snapshot copies the current table view into the open segment info.
func (s *Segmenter) snapshot() {
	s.info.PATVersion = s.st.PATVersion
	s.info.PMTVersions = map[string]int{}
	for prog, v := range s.st.PMTVersion {
		s.info.PMTVersions[fmt.Sprintf("%d", prog)] = v
	}
	s.info.Streams = s.info.Streams[:0]
	for _, st := range s.st.Streams[s.program] {
		s.info.Streams = append(s.info.Streams, StreamInfo{
			PID: st.PID, StreamType: st.Type,
			Lang: st.Lang, Registration: st.Registration,
		})
	}
	sort.Slice(s.info.Streams, func(i, j int) bool {
		return s.info.Streams[i].PID < s.info.Streams[j].PID
	})
}

func packetBytes(p *tsdemux.Packet) []byte {
	// Re-serialise the packet: the demuxer hands out parsed views, so
	// rebuild the 188 byte image for the segment file.
	pkt := make([]byte, tsdemux.PacketSize188)
	pkt[0] = 0x47
	pkt[1] = byte(p.PID >> 8)
	if p.PUSI {
		pkt[1] |= 0x40
	}
	pkt[2] = byte(p.PID)
	pkt[3] = 0x10 | (p.CC & 0x0F)
	n := copy(pkt[4:], p.Payload)
	for i := 4 + n; i < len(pkt); i++ {
		pkt[i] = 0xFF
	}
	return pkt
}

// open starts a new segment file and rewrites the current PAT/PMT
// sections at its head.
func (s *Segmenter) open() error {
	idx := len(s.segments)
	name := fmt.Sprintf("seg-%04d.ts", idx)
	f, err := os.Create(filepath.Join(s.dir, name))
	if err != nil {
		return err
	}
	s.f = f
	s.info = &Info{
		Index:   idx,
		File:    name,
		StartMS: s.lastMS,
	}
	s.content = 0
	s.snapshot()
	s.wrotePMT = false
	if len(s.st.CurrentPAT) > 0 {
		if _, err := f.Write(psi.MarshalSection(0, 0, s.st.CurrentPAT)); err != nil {
			return err
		}
	}
	if len(s.st.CurrentPMT) > 0 {
		if err := s.writePMTs(); err != nil {
			return err
		}
		s.wrotePMT = true
	}
	return nil
}

// writePMTs appends the current PMT sections of all programs.
func (s *Segmenter) writePMTs() error {
	progs := make([]int, 0, len(s.st.CurrentPMT))
	for prog := range s.st.CurrentPMT {
		progs = append(progs, int(prog))
	}
	sort.Ints(progs)
	for _, prog := range progs {
		pmtPID := s.st.PMTPID[uint16(prog)]
		if _, err := s.f.Write(psi.MarshalSection(pmtPID, 0, s.st.CurrentPMT[uint16(prog)])); err != nil {
			return err
		}
	}
	return nil
}

func (s *Segmenter) cut() error {
	if err := s.f.Close(); err != nil {
		return err
	}
	s.f = nil
	s.segments = append(s.segments, *s.info)
	s.info = nil
	return nil
}

// Finish closes the current segment and writes manifest.json.
func (s *Segmenter) Finish(input string) (*Manifest, error) {
	if s.info != nil {
		if s.info.Packets == 0 {
			s.f.Close()
			os.Remove(filepath.Join(s.dir, s.info.File))
		} else if err := s.cut(); err != nil {
			return nil, err
		}
		s.info = nil
	}
	m := &Manifest{Input: input, Program: s.program, MS: s.ms, Segments: s.segments}
	f, err := os.Create(filepath.Join(s.dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return m, nil
}
