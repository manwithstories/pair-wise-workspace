package scan

import (
	"os"
	"path/filepath"
	"testing"

	"mediaslice/internal/fixture"
	"mediaslice/media"
)

func buildFixture(t *testing.T, dir, id string, sparse bool) (string, string, *fixture.Plan) {
	t.Helper()
	spec := fixture.DefaultSpec(id, 42, media.FromSeconds(12), media.FromSeconds(2))
	if sparse {
		spec.KeyframeEveryN = 3 // keyframe only every 6 seconds
	}
	plan := fixture.BuildPlan(spec)
	mp4Path := filepath.Join(dir, id+".mp4")
	rtpPath := filepath.Join(dir, id+".rtp")
	if err := fixture.WriteMP4(mp4Path, plan); err != nil {
		t.Fatal(err)
	}
	if err := fixture.WriteRTPSidecar(rtpPath, plan); err != nil {
		t.Fatal(err)
	}
	return mp4Path, rtpPath, plan
}

func TestScanValidFixture(t *testing.T) {
	dir := t.TempDir()
	mp4Path, rtpPath, plan := buildFixture(t, dir, "800k", false)

	idx, err := ScanFile(mp4Path, rtpPath, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", idx.Errors)
	}
	if len(idx.Tracks) != 2 {
		t.Fatalf("tracks=%d want 2", len(idx.Tracks))
	}
	video := idx.videoTrack()
	if video == nil || video.Handler != "vide" {
		t.Fatal("video track not found")
	}
	if len(video.Samples) != len(plan.Video) {
		t.Fatalf("samples=%d want %d", len(video.Samples), len(plan.Video))
	}
	// Edit list retained with the non-zero media start.
	if len(video.Edits) != 1 || video.Edits[0].MediaTime != int64(plan.Spec.EditStartTicks) {
		t.Fatalf("edits not retained: %+v", video.Edits)
	}
	// VFR: at least two distinct sample durations must exist (no average
	// frame-rate assumption).
	durations := map[media.Ticks]bool{}
	for _, s := range video.Samples {
		durations[s.Duration] = true
	}
	if len(durations) < 2 {
		t.Fatal("expected VFR with multiple distinct sample durations")
	}
	// RTP unwrap + drift stats.
	if video.RTP == nil || video.RTP.WrapCount == 0 {
		t.Fatal("expected RTP wrap events")
	}
	if video.RTP.Packets != len(video.Samples) {
		t.Fatalf("rtp packets=%d", video.RTP.Packets)
	}
	if video.RTP.TotalCorrection == 0 {
		t.Fatal("drift correction never applied")
	}
	// Every sample byte range lies inside mdat.
	for _, s := range video.Samples {
		if s.Offset < idx.MdatStart || s.Offset+int64(s.Size) > idx.MdatEnd {
			t.Fatalf("sample %d outside mdat", s.Index)
		}
	}
}

func TestScanTruncatedBoxLocalization(t *testing.T) {
	dir := t.TempDir()
	mp4Path, _, _ := buildFixture(t, dir, "800k", false)

	raw, err := os.ReadFile(mp4Path)
	if err != nil {
		t.Fatal(err)
	}
	// Cut 100 bytes off the end: the final box is now truncated.
	cut := mp4Path + ".trunc"
	if err := os.WriteFile(cut, raw[:len(raw)-100], 0o600); err != nil {
		t.Fatal(err)
	}
	idx, err := ScanFile(cut, "", DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	e := idx.Errors.First(media.KindTruncatedBox)
	if e == nil {
		t.Fatalf("truncation not detected: %v", idx.Errors)
	}
	if e.ErrorBytes > media.MaxLocateErrorBytes {
		t.Fatalf("locate error bytes %d > 16", e.ErrorBytes)
	}
	if e.ByteOffset < 0 || e.ByteOffset >= int64(len(raw)) {
		t.Fatalf("bad offset %d", e.ByteOffset)
	}
}

func TestScanBadSampleTableOffset(t *testing.T) {
	dir := t.TempDir()
	mp4Path, _, _ := buildFixture(t, dir, "800k", false)
	raw, err := os.ReadFile(mp4Path)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the stco chunk offset (first 4-byte entry after the box
	// header + full-box header + entry count).
	pos := locateBox(t, raw, "stco")
	entry := pos + 8 + 4 + 4
	raw[entry] = 0xff
	raw[entry+1] = 0xff
	corrupt := filepath.Join(dir, "bad.mp4")
	if err := os.WriteFile(corrupt, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	idx, err := ScanFile(corrupt, "", DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if e := idx.Errors.First(media.KindBadSampleTable); e == nil {
		t.Fatalf("bad sample table not detected: %v", idx.Errors)
	} else if e.ErrorBytes > media.MaxLocateErrorBytes {
		t.Fatalf("locate error bytes %d > 16", e.ErrorBytes)
	}
	// The other (audio) track must still have parsed samples.
	if len(idx.Tracks) < 2 {
		t.Fatal("sibling track result lost")
	}
}

func TestScanNegativeEditListDiagnostic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "neg.mp4")
	writeNegativeEditMP4(t, path)
	idx, err := ScanFile(path, "", DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	video := idx.videoTrack()
	if video == nil || len(video.Edits) == 0 {
		t.Fatal("edit list missing")
	}
	var foundNeg bool
	for _, e := range video.Edits {
		if e.MediaTime < 0 {
			foundNeg = true
		}
	}
	if !foundNeg {
		t.Fatal("negative media time edit not retained")
	}
}

func TestScanMissingTrack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.mp4")
	// ftyp + moov with mvhd only, no trak.
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	bw := fixture.NewBoxWriter(f)
	mustStartEnd(bw, "ftyp", []byte("isom\x00\x00\x02\x00isom"))
	must(bw.Start("moov"))
	must(bw.Start("mvhd"))
	must(bw.FullBox(0, 0))
	_, _ = bw.Write(make([]byte, 96))
	must(bw.End())
	must(bw.End())
	must(f.Close())

	idx, err := ScanFile(path, "", DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if e := idx.Errors.First(media.KindMissingTrack); e == nil {
		t.Fatalf("missing track not detected: %v", idx.Errors)
	}
}

func TestFingerprintRejectsModifiedInput(t *testing.T) {
	dir := t.TempDir()
	mp4Path, rtpPath, _ := buildFixture(t, dir, "800k", false)
	idx, err := ScanFile(mp4Path, rtpPath, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if mm := VerifyIndex(idx, mp4Path); mm != nil {
		t.Fatalf("fresh index failed verification: %v", mm)
	}
	// Change one payload byte; the input hash must now mismatch.
	raw, _ := os.ReadFile(mp4Path)
	raw[len(raw)-200] ^= 0xff
	if err := os.WriteFile(mp4Path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if mm := VerifyIndex(idx, mp4Path); mm == nil || mm.Field != "input_sha256" {
		t.Fatalf("expected input_sha256 mismatch, got %v", mm)
	}
}

func TestSafeJoinRejectsTraversal(t *testing.T) {
	if _, err := SafeJoin("/tmp/work", "../escape.mp4"); err == nil {
		t.Fatal("traversal accepted")
	}
	if _, err := SafeJoin("/tmp/work", "/etc/passwd"); err == nil {
		t.Fatal("absolute path accepted")
	}
	got, err := SafeJoin("/tmp/work", "a/b/../c.mp4")
	if err != nil || got != "/tmp/work/a/c.mp4" {
		t.Fatalf("safe relative path failed: %s %v", got, err)
	}
}
