// Package slicer cuts a transport stream into self-contained program segments.
package slicer

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"tsaudit/internal/psi"
	"tsaudit/internal/ts"
)

// Config controls segment extraction.
type Config struct {
	In        string
	Program   uint16
	SegmentMS int
	OutDir    string
	IndexPath string
}

// Segment is one index entry.
type Segment struct {
	File        string   `json:"file"`
	StartPacket int64    `json:"start_pkt"`
	EndPacket   int64    `json:"end_pkt"`
	PCRStart    float64  `json:"pcr_start"`
	PIDCount    int      `json:"pid_count"`
	PIDs        []uint16 `json:"pids"`
	PATVersion  uint8    `json:"pat_version"`
	PMTVersion  uint8    `json:"pmt_version"`
}

// Index is the slicer output manifest.
type Index struct {
	Input     string    `json:"input"`
	Program   uint16    `json:"program"`
	SegmentMS int       `json:"segment_ms"`
	Segments  []Segment `json:"segments"`
}

type plan struct {
	start    int64
	end      int64
	pcr      float64
	patRaw   []byte
	pmtRaw   []byte
	pmtPID   uint16
	dataPIDs map[uint16]bool
	patVer   uint8
	pmtVer   uint8
}

type firstPass struct {
	cfg           Config
	plans         []*plan
	knownPMT      map[uint16]bool
	currentPMTPID uint16
	patRaw        []byte
	pmtRaw        []byte
	pcrPID        uint16
	dataPIDs      map[uint16]bool
	patVer        uint8
	pmtVer        uint8
	havePAT       bool
	havePMT       bool
	lastPCR       float64
	firstPCR      float64
	havePCR       bool
	nextGrid      float64
	packetCount   int64
}

// Run slices the selected program and writes TS segments plus index JSON.
func Run(cfg Config) (*Index, error) {
	if cfg.SegmentMS <= 0 {
		return nil, fmt.Errorf("segment-ms must be > 0")
	}
	fp, err := scan(cfg)
	if err != nil {
		return nil, err
	}
	if !fp.havePAT || !fp.havePMT {
		return nil, fmt.Errorf("program %d has no PAT/PMT", cfg.Program)
	}
	fp.finalize()
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return nil, err
	}
	idx, err := writeSegments(cfg, fp)
	if err != nil {
		return nil, err
	}
	if cfg.IndexPath != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.IndexPath), 0o755); err != nil {
			return nil, err
		}
		b, _ := json.MarshalIndent(idx, "", "  ")
		if err := os.WriteFile(cfg.IndexPath, append(b, '\n'), 0o644); err != nil {
			return nil, err
		}
	}
	return idx, nil
}

func scan(cfg Config) (*firstPass, error) {
	f, err := os.Open(cfg.In)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fp := &firstPass{cfg: cfg, knownPMT: make(map[uint16]bool), dataPIDs: make(map[uint16]bool)}
	fp.plans = append(fp.plans, &plan{start: 0, pcr: 0, dataPIDs: make(map[uint16]bool)})
	reasm := psi.NewReassembler(fp.onSection)
	rd, err := ts.NewReader(f)
	if err != nil {
		return nil, err
	}
	for {
		p, rerr := rd.Next()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
		fp.packetCount = p.Index + 1
		if fp.havePCR && p.Header.PID == fp.pcrPID && p.Header.AdaptationField && p.Adapt.HasPCR {
			pcr := float64(p.Adapt.PCRBase) / 90.0
			fp.lastPCR = pcr
			if pcr >= fp.nextGrid-0.5 {
				fp.addBoundary(p.Index, pcr)
				for pcr >= fp.nextGrid-0.5 {
					fp.nextGrid += float64(cfg.SegmentMS)
				}
			}
		} else if !fp.havePCR && p.Header.PID == fp.pcrPID && p.Header.AdaptationField && p.Adapt.HasPCR {
			fp.firstPCR, fp.havePCR = float64(p.Adapt.PCRBase)/90.0, true
			fp.nextGrid = fp.firstPCR + float64(cfg.SegmentMS)
		}
		if p.Header.PID == 0 || fp.knownPMT[p.Header.PID] {
			reasm.Feed(p.Header.PID, p.Header.PayloadUnitStart, p.Payload, p.Index)
		}
	}
	return fp, nil
}

func (fp *firstPass) onSection(sec *psi.Section) {
	switch sec.TableID {
	case psi.TableIDPAT:
		p, err := psi.ParsePAT(sec)
		if err != nil || !p.CurrentNext {
			return
		}
		changed := fp.havePAT && fp.patVer != p.Version
		fp.patRaw, fp.patVer, fp.havePAT = sec.Raw, p.Version, true
		for _, pr := range p.Programs {
			if !pr.IsNIT {
				fp.knownPMT[pr.PID] = true
			}
			if pr.ProgramNumber == fp.cfg.Program {
				fp.currentPMTPID = pr.PID
			}
		}
		if changed {
			fp.addBoundary(sec.StartPacket, fp.lastPCR)
		}
	case psi.TableIDPMT:
		p, err := psi.ParsePMT(sec)
		if err != nil || p.ProgramNumber != fp.cfg.Program || !p.CurrentNext {
			return
		}
		changed := fp.havePMT && fp.pmtVer != p.Version
		fp.pmtRaw, fp.pmtVer, fp.havePMT = sec.Raw, p.Version, true
		fp.pcrPID = p.PCRPID
		fp.dataPIDs = make(map[uint16]bool)
		for _, es := range p.Streams {
			fp.dataPIDs[es.PID] = true
		}
		if p.PCRPID != 0x1fff {
			fp.dataPIDs[p.PCRPID] = true
		}
		if changed {
			fp.addBoundary(sec.StartPacket, fp.lastPCR)
		} else {
			fp.updateCurrentPlan()
		}
	}
}

func (fp *firstPass) addBoundary(start int64, pcr float64) {
	if start <= 0 || start >= fp.packetCount {
		return
	}
	last := fp.plans[len(fp.plans)-1]
	if start <= last.start {
		return
	}
	np := &plan{start: start, pcr: pcr, patRaw: clone(fp.patRaw), pmtRaw: clone(fp.pmtRaw), pmtPID: fp.currentPMTPID,
		dataPIDs: clonePIDs(fp.dataPIDs), patVer: fp.patVer, pmtVer: fp.pmtVer}
	fp.plans = append(fp.plans, np)
}

func (fp *firstPass) updateCurrentPlan() {
	last := fp.plans[len(fp.plans)-1]
	last.patRaw, last.pmtRaw = clone(fp.patRaw), clone(fp.pmtRaw)
	last.pmtPID = fp.currentPMTPID
	last.patVer, last.pmtVer = fp.patVer, fp.pmtVer
	last.dataPIDs = clonePIDs(fp.dataPIDs)
}

func (fp *firstPass) finalize() {
	sort.Slice(fp.plans, func(i, j int) bool { return fp.plans[i].start < fp.plans[j].start })
	tolerance := float64(fp.cfg.SegmentMS) * 0.1
	merged := []*plan{}
	for _, p := range fp.plans {
		if len(merged) > 0 && p.start == merged[len(merged)-1].start {
			merged[len(merged)-1] = p
			continue
		}
		if len(merged) > 0 && p.start >= 0 && p.pcr-merged[len(merged)-1].pcr < tolerance && p.start > 0 {
			merged[len(merged)-1] = p
			continue
		}
		merged = append(merged, p)
	}
	fp.plans = merged
	for i := range fp.plans {
		if fp.plans[i].pcr == 0 {
			fp.plans[i].pcr = fp.firstPCR
		}
		if i+1 < len(fp.plans) {
			fp.plans[i].end = fp.plans[i+1].start - 1
		} else {
			fp.plans[i].end = fp.packetCount - 1
		}
	}
}
