package ingest_test

import (
	"errors"
	"testing"

	"mediad/internal/fixture"
	"mediad/internal/ingest"
	"mediad/internal/mediaerr"
	"mediad/internal/timeline"
)

func TestScanCleanFixtureReportsNoDamage(t *testing.T) {
	spec := fixture.DemoRenditionSpecs()[0]
	ts, err := fixture.CleanRendition(73021, spec)
	if err != nil {
		t.Fatal(err)
	}
	recs, report, err := ingest.ScanBytes(ts)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) == 0 {
		t.Fatal("expected reassembled PES records")
	}
	if report.FirstBadOffset != -1 {
		t.Fatalf("unexpected bad offset %d", report.FirstBadOffset)
	}
	if report.Gaps != nil {
		t.Fatalf("unexpected CC gaps: %+v", report.Gaps)
	}
}

func TestScanReportsFirstBadPacketOffsetAndResyncs(t *testing.T) {
	spec := fixture.DemoRenditionSpecs()[0]
	clean, err := fixture.CleanRendition(73021, spec)
	if err != nil {
		t.Fatal(err)
	}
	n := fixture.DamagePacketIndex()
	damaged, injected := fixture.DamageSync(clean, n)
	_, report, err := ingest.ScanBytes(damaged)
	if err != nil {
		t.Fatalf("one bad sync should be resynchronizable: %v", err)
	}
	if report.FirstBadOffset != injected {
		t.Fatalf("first_bad_offset=%d want %d", report.FirstBadOffset, injected)
	}
	if report.FirstBadOffset%ingest.PacketSize != 0 {
		t.Fatalf("offset %d is not packet aligned", report.FirstBadOffset)
	}
	errorPackets := (report.FirstBadOffset - injected) / ingest.PacketSize
	if errorPackets != 0 {
		t.Fatalf("offset_error_packets=%d want 0", errorPackets)
	}
	if report.ScannedPackets < n {
		t.Fatalf("scanned packets=%d did not recover after damage", report.ScannedPackets)
	}
}

func TestContinuityGapRecordedOnce(t *testing.T) {
	ts := fixture.CCJumpSample()
	s := ingest.NewScanner()
	if _, err := s.Scan(ts); err != nil {
		t.Fatal(err)
	}
	rep := s.Report()
	var videoGaps []ingest.CCGap
	for _, g := range rep.Gaps {
		if g.PID == fixture.VideoPID {
			videoGaps = append(videoGaps, g)
		}
	}
	if len(videoGaps) != 1 {
		t.Fatalf("video gaps=%+v want exactly one", videoGaps)
	}
}

func TestBadCRCPSIVersionDoesNotReplaceOldTable(t *testing.T) {
	ts := fixture.BadCRCPSISample()
	s := ingest.NewScanner()
	if _, err := s.Scan(ts); err != nil {
		t.Fatal(err)
	}
	if s.Tables.PAT == nil {
		t.Fatal("expected good PAT")
	}
	pmt := s.Tables.PMTs[fixture.PMTPID]
	if pmt == nil {
		t.Fatal("expected good PMT")
	}
	if pmt.Version != 0 {
		t.Fatalf("PMT version=%d want retained version 0 after bad v1 CRC", pmt.Version)
	}
	if s.Report().PSIRejected == 0 {
		t.Fatal("expected rejected PSI candidate")
	}
}

func TestTruncatedFixtureReturnsStructuredTail(t *testing.T) {
	spec := fixture.DemoRenditionSpecs()[0]
	clean, err := fixture.CleanRendition(73021, spec)
	if err != nil {
		t.Fatal(err)
	}
	truncated := clean[:len(clean)-37]
	_, report, err := ingest.ScanBytes(truncated)
	var se *mediaerr.Error
	if !errors.As(err, &se) || se.Kind != mediaerr.KindTruncated {
		t.Fatalf("err=%v want structured truncation", err)
	}
	if report.TruncatedOffset < 0 || report.TrailingBytes == 0 {
		t.Fatalf("report=%+v missing truncation context", report)
	}
}

func TestPCRWrapRecordsUnwrapMonotonically(t *testing.T) {
	recs, _, err := ingest.ScanBytes(fixture.PCRWrapSample())
	if err != nil {
		t.Fatal(err)
	}
	var u timeline.Unwrapper
	var last int64
	seen := 0
	for _, r := range recs {
		if !r.PTS.Valid {
			continue
		}
		got, err := u.Unwrap33(r.PTS.Value)
		if err != nil {
			t.Fatal(err)
		}
		if seen > 0 && got <= last {
			t.Fatalf("unwrapped PTS not monotonic: %d after %d", got, last)
		}
		last = got
		seen++
	}
	if seen < 2 {
		t.Fatalf("expected wrapped PTS records, got %d", seen)
	}
}
