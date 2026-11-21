package pipeline

import (
	"bytes"
	"io"
	"testing"

	"mkvarchive/internal/fixture"
)

func TestArchiveStandardSuccess(t *testing.T) {
	sample := fixture.Generate(fixture.CaseStandard, fixture.DemoClusters)
	var dst bytes.Buffer
	res, err := Archive(bytes.NewReader(sample.Data), &dst, t.TempDir(), DefaultOptions())
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if res.Outcome != OutcomeSuccess || res.Outcome.ExitCode() != 0 {
		t.Fatalf("want success/0, got %v code=%d", res.Outcome, res.Outcome.ExitCode())
	}
	if res.RecoveredBlocks != sample.BlockCount {
		t.Fatalf("recovered %d want %d", res.RecoveredBlocks, sample.BlockCount)
	}
	if !res.Quality.PayloadSHA256Match || res.Quality.QualityBlocks != sample.BlockCount {
		t.Fatalf("quality wrong: %+v", res.Quality)
	}
	if len(res.Manifest.Tracks) < 2 {
		t.Fatalf("manifest tracks: %+v", res.Manifest.Tracks)
	}
	// Bitrate must land inside the acceptance band around 12000 kbps.
	if res.Quality.TargetBitrateKbps != 12000 ||
		res.Quality.BitrateDeviationPct > 1.0 {
		t.Fatalf("bitrate out of band: %+v", res.Quality)
	}
}

func TestArchiveTruncatedIsPartial(t *testing.T) {
	sample := fixture.Generate(fixture.CaseTruncated, 30)
	var dst bytes.Buffer
	res, err := Archive(bytes.NewReader(sample.Data), &dst, t.TempDir(), DefaultOptions())
	if err != nil {
		t.Fatalf("archive should tolerate tail damage: %v", err)
	}
	if res.Outcome != OutcomePartial || res.Outcome.ExitCode() != 2 {
		t.Fatalf("want partial/2, got %v code=%d", res.Outcome, res.Outcome.ExitCode())
	}
	if res.RecoveredBlocks != sample.BlockCount || res.FirstErrorOffset <= 0 {
		t.Fatalf("recovery evidence wrong: recovered=%d offset=%d",
			res.RecoveredBlocks, res.FirstErrorOffset)
	}
	if res.FirstErrorCode != "EBML_TRUNCATED_HEADER" {
		t.Fatalf("first error code: %s", res.FirstErrorCode)
	}
}

func TestArchiveDeterministic(t *testing.T) {
	sample := fixture.Generate(fixture.CaseStandard, 20)
	run := func() (*Result, []byte) {
		var dst bytes.Buffer
		res, err := Archive(bytes.NewReader(sample.Data), &dst, t.TempDir(), DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		return res, dst.Bytes()
	}
	r1, b1 := run()
	r2, b2 := run()
	if !bytes.Equal(b1, b2) {
		t.Fatal("archived bytes must be identical across runs")
	}
	if r1.Manifest.SchemaVersion != r2.Manifest.SchemaVersion ||
		len(r1.Manifest.Tracks) != len(r2.Manifest.Tracks) {
		t.Fatal("manifest structure diverged")
	}
	for i := range r1.Manifest.Tracks {
		if r1.Manifest.Tracks[i] != r2.Manifest.Tracks[i] {
			t.Fatalf("track range %d diverged", i)
		}
	}
}

func TestVerifyRoundTrip(t *testing.T) {
	sample := fixture.Generate(fixture.CaseStandard, 25)
	var dst bytes.Buffer
	res, err := Archive(bytes.NewReader(sample.Data), &dst, t.TempDir(), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	durationNS := fixture.DurationNS(25)
	vr, err := Verify(bytes.NewReader(sample.Data), bytes.NewReader(dst.Bytes()),
		res.Manifest, 12000, durationNS)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !vr.PayloadSHA256Match || vr.ManifestAlignmentMS > 1 ||
		vr.QualityBlocks != sample.BlockCount {
		t.Fatalf("verify evidence wrong: %+v", vr)
	}
	if vr.BitrateDeviationPct > 1.0 {
		t.Fatalf("bitrate deviation: %+v", vr)
	}
}

func TestArchiveStreamsFromPipe(t *testing.T) {
	sample := fixture.Generate(fixture.CaseStandard, 15)
	pr, pw := io.Pipe()
	go func() {
		// Feed the source in small chunks; a non-streaming implementation
		// would need the whole slice available before parsing could start.
		for off := 0; off < len(sample.Data); off += 4096 {
			end := off + 4096
			if end > len(sample.Data) {
				end = len(sample.Data)
			}
			_, _ = pw.Write(sample.Data[off:end])
		}
		_ = pw.Close()
	}()
	var dst bytes.Buffer
	res, err := Archive(pr, &dst, t.TempDir(), DefaultOptions())
	if err != nil {
		t.Fatalf("streaming archive: %v", err)
	}
	if res.Outcome != OutcomeSuccess || res.RecoveredBlocks != sample.BlockCount {
		t.Fatalf("streaming result wrong: %+v", res)
	}
	if dst.Len() == 0 {
		t.Fatal("no archive bytes written from piped input")
	}
}

func TestMetadataWarningsReachResult(t *testing.T) {
	sample := fixture.Generate(fixture.CaseColorMissing, 10)
	var dst bytes.Buffer
	res, err := Archive(bytes.NewReader(sample.Data), &dst, t.TempDir(), DefaultOptions())
	if err != nil {
		t.Fatalf("retain policy must succeed: %v", err)
	}
	found := false
	for _, w := range res.Warnings {
		if w.Code == "COLOR_METADATA_MISSING" {
			found = true
		}
	}
	if !found {
		t.Fatalf("metadata warning missing from result: %+v", res.Warnings)
	}
}
