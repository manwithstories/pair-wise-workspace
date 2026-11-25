package manifest

import (
	"path/filepath"
	"testing"

	"archivectl/internal/container"
)

func TestLedgerRoundTripAndCheck(t *testing.T) {
	events := []container.Event{
		{Kind: container.EventChapter, TrackID: 3, TimeNS: 0, Timescale: 1000, Hash: "a"},
		{Kind: container.EventEMSG, TrackID: 0, TimeNS: 1500000000, Timescale: 1000, Hash: "b"},
	}
	entries := BuildEntries(events)
	path := filepath.Join(t.TempDir(), "list.csv")
	if err := WriteCSV(path, entries); err != nil {
		t.Fatal(err)
	}
	read, err := ReadCSV(path)
	if err != nil {
		t.Fatal(err)
	}
	maxMS, mismatch, err := CheckEvents(read, events)
	if err != nil {
		t.Fatalf("check events: %v", err)
	}
	if mismatch != 0 || maxMS > 0.001 {
		t.Fatalf("maxMS=%f mismatch=%d", maxMS, mismatch)
	}
	h1, err := HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCSV(path, entries); err != nil {
		t.Fatal(err)
	}
	h2, _ := HashFile(path)
	if h1 != h2 {
		t.Fatal("ledger hash is not deterministic")
	}
}

func TestJournalRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.journal")
	j := Journal{Version: 1, InputSHA256: "in", Window: 64, TargetKBPS: 1200, OutputSHA256: "out"}
	if err := SaveJournal(path, j); err != nil {
		t.Fatal(err)
	}
	got, err := LoadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.OutputSHA256 != "out" {
		t.Fatalf("journal=%+v", got)
	}
	if err := ValidateResume(got, Journal{InputSHA256: "in", Window: 64, TargetKBPS: 1200}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateResume(got, Journal{InputSHA256: "different", Window: 64}); err == nil {
		t.Fatal("expected parameter conflict")
	}
}
