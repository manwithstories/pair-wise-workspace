package model

import (
	"testing"

	"tsaudit/internal/psi"
)

func TestPATVersionWrapAndCurrentNext(t *testing.T) {
	tracker := newPATTracker()
	tracker.Ingest(&psi.PAT{TransportStreamID: 1, Version: 31, CurrentNext: true,
		Programs: []psi.PATProgram{{ProgramNumber: 1, PID: 0x100}}, StartPacket: 1})
	tracker.Ingest(&psi.PAT{TransportStreamID: 1, Version: 0, CurrentNext: true,
		Programs: []psi.PATProgram{{ProgramNumber: 1, PID: 0x110}}, StartPacket: 2})
	tracker.Ingest(&psi.PAT{TransportStreamID: 1, Version: 0, CurrentNext: true,
		Programs: []psi.PATProgram{{ProgramNumber: 1, PID: 0x110}}, StartPacket: 3})
	tracker.Ingest(&psi.PAT{TransportStreamID: 1, Version: 5, CurrentNext: false,
		Programs: []psi.PATProgram{{ProgramNumber: 1, PID: 0x120}}, StartPacket: 4})
	if tracker.VersionChanges != 1 || tracker.CurrentVersion != 0 || tracker.Programs[0].PID != 0x110 {
		t.Fatalf("after current_next=0: changes=%d version=%d programs=%+v", tracker.VersionChanges, tracker.CurrentVersion, tracker.Programs)
	}
	tracker.Ingest(&psi.PAT{TransportStreamID: 1, Version: 5, CurrentNext: true,
		Programs: []psi.PATProgram{{ProgramNumber: 1, PID: 0x120}}, StartPacket: 5})
	if tracker.VersionChanges != 2 || tracker.CurrentVersion != 5 || tracker.Programs[0].PID != 0x120 {
		t.Fatalf("after applicable next table: changes=%d version=%d programs=%+v", tracker.VersionChanges, tracker.CurrentVersion, tracker.Programs)
	}
	if len(tracker.Versions) != 3 || tracker.Versions[0].FirstPacket != 1 {
		t.Fatalf("unexpected version ledger: %+v", tracker.Versions)
	}
}

func TestPMTRemapKeepsHistoricalPID(t *testing.T) {
	state := NewState()
	state.IngestPMT(0x200, &psi.PMT{ProgramNumber: 2, Version: 0, CurrentNext: true, PCRPID: 0x201,
		Streams: []psi.ElementaryStream{{StreamType: 0x0f, PID: 0x202, Language: "zho"}}, StartPacket: 1})
	state.IngestPMT(0x210, &psi.PMT{ProgramNumber: 2, Version: 1, CurrentNext: true, PCRPID: 0x201,
		Streams: []psi.ElementaryStream{{StreamType: 0x0f, PID: 0x212, Language: "zho"}}, StartPacket: 2})
	tracker := state.PMTs[2]
	if tracker.VersionChanges != 1 || tracker.CurrentPMTPID != 0x210 {
		t.Fatalf("unexpected PMT tracker: %+v", tracker)
	}
	decl := state.Declarations()
	if decl[0x202].Program != 2 || decl[0x202].Kind != "ES" {
		t.Fatalf("historical PID lost: %+v", decl[0x202])
	}
	if decl[0x212].Language != "zho" || decl[0x212].Program != 2 {
		t.Fatalf("new PID not declared: %+v", decl[0x212])
	}
}
