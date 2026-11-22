package ingest_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"mediarun/internal/ingest"
	"mediarun/internal/mediaerr"
	"mediarun/internal/timebase"
	"mediarun/testdata"
)

func workDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	return dir
}

func drain(t *testing.T, s ingest.Streamer) (total int64, byTrack map[string]int64, bytes int64) {
	t.Helper()
	byTrack = map[string]int64{}
	for {
		ev, err := s.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next unexpected error: %v", err)
		}
		got, err := io.Copy(io.Discard, ev.Payload)
		if err != nil {
			t.Fatalf("payload read: %v", err)
		}
		if got != ev.Size {
			t.Fatalf("payload bytes %d != declared size %d", got, ev.Size)
		}
		total++
		bytes += got
		byTrack[ev.TrackID]++
	}
	return total, byTrack, bytes
}

func TestMP4RoundTrip(t *testing.T) {
	path := filepath.Join(workDir(t, "mp4"), "in.mp4")
	spec := testdata.FixtureSpec{
		Path: path, Format: "mp4", VideoSamples: 120, AudioSamples: 60,
		VideoScale: 30000, FrameDur: 1001, Width: 320, Height: 180, AudioRate: 48000,
	}
	if err := testdata.Write(spec); err != nil {
		t.Fatal(err)
	}
	s, err := ingest.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	info := s.Info()
	if info.Format != "mp4" || len(info.Tracks) != 2 {
		t.Fatalf("info = %+v", info)
	}
	var v, a *ingest.TrackDesc
	for i := range info.Tracks {
		tr := &info.Tracks[i]
		switch tr.Kind {
		case ingest.KindVideo:
			v = tr
		case ingest.KindAudio:
			a = tr
		}
	}
	if v == nil || a == nil || v.Codec != "avc1" || a.Codec != "mp4a" {
		t.Fatalf("codec/kind wrong: %+v %+v", v, a)
	}
	if v.Width != 320 || v.Height != 180 || a.Channels != 2 || v.Timescale != 30000 {
		t.Fatalf("geometry/rate wrong: %+v %+v", v, a)
	}
	if v.SampleCount != 120 || a.SampleCount != 60 {
		t.Fatalf("sample counts wrong: %d %d", v.SampleCount, a.SampleCount)
	}
	total, byTrack, _ := drain(t, s)
	if total != 180 || byTrack[v.ID] != 120 || byTrack[a.ID] != 60 {
		t.Fatalf("drained total=%d per=%v", total, byTrack)
	}
}

func TestMXFRoundTrip(t *testing.T) {
	path := filepath.Join(workDir(t, "mxf"), "in.mxf")
	spec := testdata.FixtureSpec{
		Path: path, Format: "mxf", VideoSamples: 50, AudioSamples: 40,
		VideoScale: 25, FrameDur: 1, Width: 640, Height: 360, AudioRate: 48000,
	}
	if err := testdata.Write(spec); err != nil {
		t.Fatal(err)
	}
	s, err := ingest.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if s.Info().Format != "mxf" {
		t.Fatalf("format = %s", s.Info().Format)
	}
	total, byTrack, _ := drain(t, s)
	if total != 90 {
		t.Fatalf("total = %d, want 90", total)
	}
	if len(byTrack) != 2 {
		t.Fatalf("tracks = %v", byTrack)
	}
}

func TestTruncatedMP4ReportsOffset(t *testing.T) {
	path := filepath.Join(workDir(t, "trunc"), "in.mp4")
	spec := testdata.FixtureSpec{
		Path: path, Format: "mp4", VideoSamples: 80, AudioSamples: 20,
		Width: 320, Height: 180, Injection: "truncated-box",
	}
	if err := testdata.Write(spec); err != nil {
		t.Fatal(err)
	}
	s, err := ingest.Open(path)
	if err != nil {
		// Header tables may reject first; either way it must be structured.
		if !mediaerr.Is(err, mediaerr.CodeTruncatedBox) &&
			!mediaerr.Is(err, mediaerr.CodeUnknownStructure) {
			t.Fatalf("want truncation error, got %v", err)
		}
		return
	}
	defer s.Close()
	for {
		_, err := s.Next()
		if err == io.EOF {
			t.Fatal("expected truncation error, got clean EOF")
		}
		if err != nil {
			se, ok := err.(*mediaerr.Error)
			if !ok || se.Offset < 0 || se.Path != path {
				t.Fatalf("error must carry offset and path, got %v", err)
			}
			return
		}
	}
}

func TestTruncatedMXFReportsKLVOffset(t *testing.T) {
	path := filepath.Join(workDir(t, "truncklv"), "in.mxf")
	spec := testdata.FixtureSpec{
		Path: path, Format: "mxf", VideoSamples: 30, AudioSamples: 10,
		Width: 320, Height: 180, Injection: "truncated-klv",
	}
	if err := testdata.Write(spec); err != nil {
		t.Fatal(err)
	}
	s, err := ingest.Open(path)
	if err != nil {
		t.Fatalf("partition should parse, got %v", err)
	}
	defer s.Close()
	var sawErr error
	for {
		ev, err := s.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			sawErr = err
			break
		}
		if _, err := io.Copy(io.Discard, ev.Payload); err != nil {
			sawErr = err
			break
		}
	}
	if !mediaerr.Is(sawErr, mediaerr.CodeTruncatedKLV) {
		t.Fatalf("want TRUNCATED_KLV, got %v", sawErr)
	}
	se := sawErr.(*mediaerr.Error)
	if se.Offset < 0 || se.Track == "" {
		t.Fatalf("KLV error must carry offset and track: %v", sawErr)
	}
}

func TestUnknownContainerRejected(t *testing.T) {
	path := filepath.Join(workDir(t, "bad"), "in.bin")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("this is not a container at all!!"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ingest.Open(path)
	if !mediaerr.Is(err, mediaerr.CodeUnknownStructure) {
		t.Fatalf("want UNKNOWN_STRUCTURE, got %v", err)
	}
}

func TestBadMagicRejected(t *testing.T) {
	path := filepath.Join(workDir(t, "magic"), "in.mp4")
	spec := testdata.FixtureSpec{
		Path: path, Format: "mp4", VideoSamples: 10, AudioSamples: 5,
		Width: 160, Height: 90, Injection: "bad-magic",
	}
	if err := testdata.Write(spec); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.Open(path); !mediaerr.Is(err, mediaerr.CodeUnknownStructure) {
		t.Fatalf("want UNKNOWN_STRUCTURE for bad magic, got %v", err)
	}
}

func TestDeterministicPayload(t *testing.T) {
	a := testdata.SampleByte(0, 7)
	b := testdata.SampleByte(0, 7)
	if a != b {
		t.Fatal("sample bytes must be reproducible")
	}
	if testdata.SampleByte(0, 7) == testdata.SampleByte(1, 7) {
		t.Fatal("distinct tracks must differ")
	}
	// Sanity: timebase used by events is the track timescale.
	tb := timebase.NewTimebase(30000)
	if tb.ToSeconds(30000).Cmp(timebase.FromInt(1)) != 0 {
		t.Fatal("30000 ticks must equal 1s")
	}
}
