package report

import (
	"fmt"
	"io"
	"os"

	"tsaudit/internal/audit"
	"tsaudit/internal/model"
	"tsaudit/internal/psi"
	"tsaudit/internal/ts"
)

// ScanFile performs a streaming packet/PSI audit and returns a JSON-ready report.
func ScanFile(path string) (*Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	state := model.NewState()
	cont := audit.NewContinuity()
	pcr := audit.NewPCRCheck()
	reasm := psi.NewReassembler(func(sec *psi.Section) {
		switch sec.TableID {
		case psi.TableIDPAT:
			if p, perr := psi.ParsePAT(sec); perr == nil {
				state.IngestPAT(p)
			}
		case psi.TableIDPMT:
			if p, perr := psi.ParsePMT(sec); perr == nil {
				state.IngestPMT(sec.PID, p)
			}
		}
	})
	rd, err := ts.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("packet framing %s: %w", path, err)
	}
	var unclaimedPSI int64
	for {
		p, rerr := rd.Next()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
		state.RecordPacket(p)
		cont.Observe(p)
		pcr.Observe(p)
		if p.Header.PID == 0 || state.IsPMTPID(p.Header.PID) {
			reasm.Feed(p.Header.PID, p.Header.PayloadUnitStart, p.Payload, p.Index)
		} else if looksLikePSI(p) {
			unclaimedPSI++
			reasm.Feed(p.Header.PID, p.Header.PayloadUnitStart, p.Payload, p.Index)
		}
	}
	stats := rd.Stats()
	rst := reasm.Stats()
	entries, orphans, zero := buildPIDMap(state)
	r := &Report{
		File:              path,
		BytesTotal:        fi.Size(),
		PacketSize:        stats.PacketSize,
		PacketsTotal:      stats.PacketCount,
		SyncLosses:        stats.SyncLosses,
		GarbageBytes:      stats.GarbageBytes,
		CRCErrors:         rst.CRCErrors,
		DiscardedSections: rst.Discarded,
		UnclaimedPSI:      unclaimedPSI,
		PAT:               buildPAT(state.PAT),
		PMTs:              buildPMTs(state),
		PIDMap:            entries,
		OrphanPMTs:        orphanPMTs(state),
		OrphanPIDs:        orphans,
		ZeroPacketES:      zero,
		Continuity:        cont.Stats(),
		PCR:               pcr.Stats(),
		NullPackets:       state.NullPkts,
	}
	return r, nil
}

func looksLikePSI(p *ts.Packet) bool {
	if !p.Header.PayloadUnitStart || len(p.Payload) < 2 {
		return false
	}
	ptr := int(p.Payload[0])
	if 1+ptr >= len(p.Payload) {
		return false
	}
	id := p.Payload[1+ptr]
	return id == psi.TableIDPAT || id == psi.TableIDPMT
}
