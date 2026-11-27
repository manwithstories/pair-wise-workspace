package fixture

import (
	"testing"

	"mediacore/internal/scan"
)

// TestTSFixtureCarriesTimedMetadata asserts the MPEG-TS fixture exposes ID3 and
// emsg records on the shared timeline.
func TestTSFixtureCarriesTimedMetadata(t *testing.T) {
	m, err := Build(Options{Dir: t.TempDir(), Name: "ts", Format: FormatTS, Segments: 4})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	sc, err := scan.Run(m.Path, scan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var id3, emsg int
	for _, rec := range sc.Meta {
		if rec.Quarantined {
			continue
		}
		switch rec.Kind {
		case "id3":
			id3++
		case "emsg":
			emsg++
		}
	}
	if id3 == 0 {
		t.Fatalf("no ID3 records in TS fixture (meta=%d)", len(sc.Meta))
	}
	if emsg == 0 {
		t.Fatalf("no emsg records in TS fixture (meta=%d)", len(sc.Meta))
	}
}

// TestTSFixtureTruncatedMetadataQuarantined asserts the truncated ID3 record is
// isolated with a located reason while the rest of the file still scans.
func TestTSFixtureTruncatedMetadataQuarantined(t *testing.T) {
	m, err := Build(Options{Dir: t.TempDir(), Name: "ts-trunc", Format: FormatTS, Segments: 4, Fault: FaultTruncate})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	sc, err := scan.Run(m.Path, scan.Options{})
	if err != nil {
		t.Fatalf("truncated TS must still scan: %v", err)
	}
	var quarantined int
	for _, rec := range sc.Meta {
		if rec.Quarantined {
			quarantined++
			if rec.Reason == "" {
				t.Fatal("quarantined record lacks a reason")
			}
			if rec.TimeUS < 0 {
				t.Fatalf("quarantined record at negative time %d", rec.TimeUS)
			}
		}
	}
	if quarantined == 0 {
		t.Fatal("truncated data sample was not quarantined")
	}
}
