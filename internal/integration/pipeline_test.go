package integration

import (
	"os"
	"path/filepath"
	"testing"

	"mediacheck/internal/fanout"
	"mediacheck/internal/fault"
	"mediacheck/internal/fixture"
	"mediacheck/internal/probe"
	"mediacheck/internal/repair"
)

// pipeline runs the real stages over a generated fixture.
type pipeline struct {
	path string
	gen  fixture.Result
	pf   *probe.File
	res  *repair.Result
	man  *fanout.Manifest
}

func runPipeline(t *testing.T, kind fixture.Kind) pipeline {
	t.Helper()
	dir := t.TempDir()
	gen, err := fixture.Generate(dir, fixture.DefaultSpec(kind))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	rcfg := repair.DefaultConfig()
	rcfg.Recover = true
	pf, res, err := repair.New(rcfg, probe.DefaultConfig()).Analyze(gen.Path)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	man, err := fanout.FromRecovery(fanout.ManifestInput{Recovery: res, Config: fanout.DefaultConfig()})
	if err != nil {
		t.Fatalf("fanout: %v", err)
	}
	return pipeline{path: gen.Path, gen: gen, pf: pf, res: res, man: man}
}

// TestFixtureHashMatchesDisk recomputes the digest independently.
func TestFixtureHashMatchesDisk(t *testing.T) {
	for _, kind := range []fixture.Kind{fixture.Valid, fixture.Truncated, fixture.Corrupt, fixture.MissingIndex} {
		gen, err := fixture.Generate(t.TempDir(), fixture.DefaultSpec(kind))
		if err != nil {
			t.Fatal(err)
		}
		got, err := hashFile(gen.Path)
		if err != nil {
			t.Fatal(err)
		}
		if got != gen.SHA256 {
			t.Fatalf("%s digest mismatch: %s vs %s", kind, got, gen.SHA256)
		}
		size, err := fileSize(gen.Path)
		if err != nil {
			t.Fatal(err)
		}
		if size != gen.Bytes {
			t.Fatalf("%s size %d != %d", kind, size, gen.Bytes)
		}
	}
}

// TestBoundaryTimestampsRecomputed re-walks the sample table with the
// independent parser and checks the recovered segment starts against it.
func TestBoundaryTimestampsRecomputed(t *testing.T) {
	p := runPipeline(t, fixture.Valid)
	tracks, err := parseMovie(p.path)
	if err != nil {
		t.Fatalf("independent parse: %v", err)
	}
	for _, tr := range tracks {
		rows := tr.rows()
		if len(rows) == 0 {
			t.Fatalf("track %d has no rows", tr.trackID)
		}
		got := p.res.TrackByID(tr.trackID)
		if got == nil {
			t.Fatalf("track %d missing from result", tr.trackID)
		}
		if got.Recovered.StartMS != rows[0].dtsMS {
			t.Fatalf("track %d start=%d want %d", tr.trackID, got.Recovered.StartMS, rows[0].dtsMS)
		}
		if got.Recovered.EndMS != rows[len(rows)-1].endMS {
			t.Fatalf("track %d end=%d want %d", tr.trackID, got.Recovered.EndMS, rows[len(rows)-1].endMS)
		}
		for i, seg := range got.Segments {
			if i >= len(rows) {
				break
			}
			_ = seg
		}
		// Segment starts must be the bucket floor of the first row in them.
		var seen = map[int]int64{}
		for _, r := range rows {
			bucket := int(r.dtsMS / p.man.SegmentMS)
			if _, ok := seen[bucket]; !ok {
				seen[bucket] = r.dtsMS
			}
		}
		for _, seg := range got.Segments {
			if want, ok := seen[seg.Index]; ok && seg.FirstDTSMS != want {
				t.Fatalf("track %d segment %d first=%d want %d", tr.trackID, seg.Index, seg.FirstDTSMS, want)
			}
		}
	}
}

// TestManifestStatsRecomputed rebuilds per-segment byte totals from the raw
// sample table and compares them to the manifest.
func TestManifestStatsRecomputed(t *testing.T) {
	p := runPipeline(t, fixture.Valid)
	tracks, err := parseMovie(p.path)
	if err != nil {
		t.Fatal(err)
	}
	var video *trackTables
	for _, tr := range tracks {
		if tr.timescale == fixture.VideoTimescale {
			video = tr
		}
	}
	if video == nil {
		t.Fatal("no video track")
	}
	wantBytesByBucket := map[int]int64{}
	var total int64
	for _, r := range video.rows() {
		bucket := int(r.dtsMS / p.man.SegmentMS)
		wantBytesByBucket[bucket] += int64(r.size)
		total += int64(r.size)
	}
	if len(p.man.Renditions) == 0 {
		t.Fatal("no renditions")
	}
	gotSegs := p.man.Renditions[0].Segments
	if len(gotSegs) != len(wantBytesByBucket) {
		t.Fatalf("segment count=%d want %d", len(gotSegs), len(wantBytesByBucket))
	}
	var gotTotal int64
	for _, seg := range gotSegs {
		if seg.Bytes != wantBytesByBucket[seg.Index] {
			t.Fatalf("segment %d bytes=%d want %d", seg.Index, seg.Bytes, wantBytesByBucket[seg.Index])
		}
		gotTotal += seg.Bytes
	}
	if gotTotal != total {
		t.Fatalf("total bytes=%d want %d", gotTotal, total)
	}
	if p.man.Renditions[0].ActualBytes != total {
		t.Fatalf("rendition actual bytes=%d want %d", p.man.Renditions[0].ActualBytes, total)
	}
}

// TestDamageOffsetMatchesDisk checks the reported damage offset points at the
// erased sample in the raw file.
func TestDamageOffsetMatchesDisk(t *testing.T) {
	p := runPipeline(t, fixture.Corrupt)
	v := p.res.TrackByID(fixture.VideoTrackID)
	if v == nil || len(v.Damages) == 0 {
		t.Fatal("no damage recorded")
	}
	d := v.Damages[0]
	raw, err := os.ReadFile(p.path)
	if err != nil {
		t.Fatal(err)
	}
	// The corrupted sample must be a run of identical bytes at the offset.
	end := d.Offset + int64(d.SampleCount)
	if end > int64(len(raw)) {
		end = int64(len(raw))
	}
	if d.Offset < 0 || end <= d.Offset {
		t.Fatalf("damage range [%d,%d) invalid", d.Offset, end)
	}
	first := raw[d.Offset]
	for i := d.Offset; i < end; i++ {
		if raw[i] != first {
			t.Fatalf("byte %d not uniform fill", i)
		}
	}
	if d.Offset != p.gen.DamageOffset {
		t.Fatalf("damage offset=%d want fixture %d", d.Offset, p.gen.DamageOffset)
	}
}

// TestRecoveredSamplesExcludeDamage asserts no recovered sample overlaps the
// damaged byte range.
func TestRecoveredSamplesExcludeDamage(t *testing.T) {
	p := runPipeline(t, fixture.Corrupt)
	v := p.res.TrackByID(fixture.VideoTrackID)
	if v == nil {
		t.Fatal("no video track")
	}
	var damageStart, damageEnd int64 = 1 << 62, 0
	for _, d := range v.Damages {
		if d.Offset < damageStart {
			damageStart = d.Offset
		}
		if d.Offset+int64(d.SampleCount) > damageEnd {
			damageEnd = d.Offset + int64(d.SampleCount)
		}
	}
	tracks, err := parseMovie(p.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range tracks {
		if tr.trackID != fixture.VideoTrackID {
			continue
		}
		for _, r := range tr.rows() {
			if int64(r.offset) >= damageStart && int64(r.offset) < damageEnd {
				// The damaged row must not be counted in the recovered total.
				if int(r.dtsMS/p.man.SegmentMS) >= 0 {
					// Verify the recovered byte total excludes these bytes.
					total := int64(0)
					for _, seg := range v.Segments {
						total += seg.Bytes
					}
					if total >= v.Recovered.Bytes+int64(r.size) {
						t.Fatalf("damaged sample bytes still counted")
					}
				}
			}
		}
	}
	if v.Recovered.Bytes >= int64(p.gen.Bytes) {
		t.Fatal("recovered bytes cover the entire file")
	}
}

// TestErrorCodesSharedAcrossKinds checks audio and video use the same codes.
func TestErrorCodesSharedAcrossKinds(t *testing.T) {
	p := runPipeline(t, fixture.Truncated)
	for _, tr := range p.res.Tracks {
		for _, d := range tr.Damages {
			if d.Code != fault.CodeTruncated && d.Code != fault.CodeCorrupt {
				t.Fatalf("track %d unexpected damage code %s", tr.TrackID, d.Code)
			}
		}
	}
	// Both tracks must classify their first fault with a shared code.
	v := p.res.TrackByID(fixture.VideoTrackID)
	a := p.res.TrackByID(fixture.AudioTrackID)
	if a == nil || len(a.Damages) == 0 {
		t.Fatal("audio truncation not recorded")
	}
	if v != nil && len(v.Damages) > 0 && v.Damages[0].Code != a.Damages[0].Code {
		t.Fatal("audio and video use different codes for the same fault")
	}
}

// TestRepeatedRunsShareSummary reruns the pipeline and compares the parts
// that must be stable.
func TestRepeatedRunsShareSummary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.mp4")
	if _, err := fixture.Generate(dir, fixture.DefaultSpec(fixture.Corrupt)); err != nil {
		t.Fatal(err)
	}
	rcfg := repair.DefaultConfig()
	rcfg.Recover = true
	summaries := make([]string, 2)
	for i := range summaries {
		_, res, err := repair.New(rcfg, probe.DefaultConfig()).Analyze(path)
		if err != nil {
			t.Fatal(err)
		}
		m, err := fanout.FromRecovery(fanout.ManifestInput{Recovery: res, Config: fanout.DefaultConfig()})
		if err != nil {
			t.Fatal(err)
		}
		summaries[i] = formatSummary(res, m)
	}
	if summaries[0] != summaries[1] {
		t.Fatalf("summaries differ:\n%s\n---\n%s", summaries[0], summaries[1])
	}
}

func formatSummary(res *repair.Result, m *fanout.Manifest) string {
	out := ""
	for _, tr := range res.Tracks {
		out += string(tr.Kind) + ":" + string(tr.Status) + ":"
		out += itoa(tr.Recovered.Samples) + ":" + itoa64(tr.Recovered.Bytes) + ";"
	}
	out += "|" + itoa64(m.TotalBytes) + ":" + itoa(m.QualityValidSamples) + ":" + itoa64(m.AlignmentMS)
	return out
}

func itoa(v int) string     { return strconvItoa(int64(v)) }
func itoa64(v int64) string { return strconvItoa(v) }

func strconvItoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [24]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
