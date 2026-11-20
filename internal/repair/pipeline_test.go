package repair_test

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"testing"

	"mediaroute/internal/index"
	"mediaroute/internal/packet"
	"mediaroute/internal/repair"
	"mediaroute/internal/testkit"
)

var testCfg = repair.Config{Window: 64, DriftPPM: 1000}

// generateRecords builds a damaged stream and parses it back into records.
func generateRecords(t *testing.T, opts testkit.Options) ([]*packet.Record, *testkit.Result) {
	t.Helper()
	var buf bytes.Buffer
	res, err := testkit.Generate(&buf, opts)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	rd := packet.NewReader(&buf)
	var recs []*packet.Record
	for {
		rec, err := rd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read record: %v", err)
		}
		recs = append(recs, rec)
	}
	return recs, res
}

func sessionOf(t *testing.T, recs []*packet.Record) *packet.Session {
	t.Helper()
	for _, rec := range recs {
		if rec.Session != nil {
			return rec.Session
		}
	}
	t.Fatal("no session record")
	return nil
}

// runOnce drives records[start:] through a fresh or restored pipeline and
// returns the output bytes, manifest and report.
func runOnce(t *testing.T, recs []*packet.Record, start int,
	st *repair.State, committed int64, buf *bytes.Buffer) (*repair.Pipeline, *repair.Report) {
	t.Helper()
	metas, err := repair.TrackMetas(sessionOf(t, recs))
	if err != nil {
		t.Fatalf("track metas: %v", err)
	}
	idxw := index.NewWriter(buf, metas)
	p, err := repair.NewPipeline(testCfg, sessionOf(t, recs), idxw)
	if err != nil {
		t.Fatalf("new pipeline: %v", err)
	}
	if st == nil {
		if err := idxw.WriteInit(); err != nil {
			t.Fatalf("write init: %v", err)
		}
	} else if err := p.Import(st, committed); err != nil {
		t.Fatalf("import: %v", err)
	}
	for _, rec := range recs[start:] {
		if rec.Session != nil {
			continue
		}
		if err := p.Process(rec); err != nil {
			t.Fatalf("process record %d: %v", rec.Index, err)
		}
	}
	if err := p.Finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	rep := p.Report()
	return p, &rep
}

func TestPipelineRecoversDamagedStream(t *testing.T) {
	opts := testkit.Options{Seed: 41, PacketsPerTrack: 7500, GOP: 60}
	recs, res := generateRecords(t, opts)
	var buf bytes.Buffer
	p, rep := runOnce(t, recs, 0, nil, 0, &buf)
	man := p.Manifest()

	if !rep.HasDamage {
		t.Fatal("expected damage to be located")
	}
	if d := int64(rep.FirstDamageSeq) - int64(res.DamagedFirstSeq); d < -1 || d > 1 {
		t.Fatalf("first damage located at %d, ground truth %d", rep.FirstDamageSeq, res.DamagedFirstSeq)
	}
	kinds := map[string]bool{}
	for _, ev := range rep.Events {
		kinds[ev.Kind.String()] = true
	}
	for _, want := range []string{"loss", "reorder", "duplicate", "conflict", "late"} {
		if !kinds[want] {
			t.Errorf("missing order event %q", want)
		}
	}
	var backward *repair.TSError
	for i, e := range rep.TSErrors {
		if e.Kind == "backward" {
			backward = &rep.TSErrors[i]
		}
	}
	if backward == nil {
		t.Fatal("missing backward timestamp error")
	} else if backward.ExtSeq != 5500 {
		t.Fatalf("backward error at seq %d, want 5500", backward.ExtSeq)
	}
	if rep.IndexEntries == 0 || len(man.Entries) != rep.IndexEntries {
		t.Fatalf("index entries: report %d manifest %d", rep.IndexEntries, len(man.Entries))
	}
	if len(man.NonRandomAccess) == 0 {
		t.Fatal("expected non-random-access intervals after video loss")
	}
	for _, e := range man.Entries {
		if e.SrcLastExtSeq < e.SrcFirstExtSeq {
			t.Fatalf("entry %d: bad source range %d..%d", e.Index, e.SrcFirstExtSeq, e.SrcLastExtSeq)
		}
		if e.TimeBase != "rtcp-sr" && e.TimeBase != "estimated" {
			t.Fatalf("entry %d: bad time base %q", e.Index, e.TimeBase)
		}
		if e.DurationNs <= 0 || e.DurationNs > 10_000_000_000 {
			t.Fatalf("entry %d: implausible duration %d ns", e.Index, e.DurationNs)
		}
	}
	// Every fragment must start at a video keyframe (random access).
	frags, err := index.Inspect(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if len(frags) < 100 {
		t.Fatalf("too few fragments: %d", len(frags))
	}
	var videoID uint32
	metas, _ := repair.TrackMetas(sessionOf(t, recs))
	for _, m := range metas {
		if m.MediaType == "video" {
			videoID = m.TrackID
		}
	}
	for _, f := range frags {
		if !f.FirstSync[videoID] {
			t.Fatalf("fragment %d does not start on a keyframe", f.Seq)
		}
	}
}

func TestPipelineDeterministicAndResumable(t *testing.T) {
	opts := testkit.Options{Seed: 7, PacketsPerTrack: 3000, GOP: 30}
	recs, _ := generateRecords(t, opts)

	var full bytes.Buffer
	pFull, _ := runOnce(t, recs, 0, nil, 0, &full)

	// Second uninterrupted run must produce identical bytes and entries.
	var again bytes.Buffer
	pAgain, _ := runOnce(t, recs, 0, nil, 0, &again)
	if !bytes.Equal(full.Bytes(), again.Bytes()) {
		t.Fatal("identical inputs produced different outputs")
	}
	if !reflect.DeepEqual(pFull.Manifest().Entries, pAgain.Manifest().Entries) {
		t.Fatal("identical inputs produced different manifests")
	}

	// Interrupt at record 5000, serialize, restore, continue: same result.
	const cut = 5000
	var part bytes.Buffer
	metas, _ := repair.TrackMetas(sessionOf(t, recs))
	idxw := index.NewWriter(&part, metas)
	p1, err := repair.NewPipeline(testCfg, sessionOf(t, recs), idxw)
	if err != nil {
		t.Fatalf("new pipeline: %v", err)
	}
	if err := idxw.WriteInit(); err != nil {
		t.Fatalf("write init: %v", err)
	}
	for _, rec := range recs[:cut] {
		if rec.Session != nil {
			continue
		}
		if err := p1.Process(rec); err != nil {
			t.Fatalf("process: %v", err)
		}
	}
	st, err := p1.Export()
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	var st2 repair.State
	if err := json.Unmarshal(raw, &st2); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	p2, _ := runOnce(t, recs, cut, &st2, p1.OutputOffset(), &part)
	if !bytes.Equal(full.Bytes(), part.Bytes()) {
		t.Fatal("resumed output differs from uninterrupted output")
	}
	if !reflect.DeepEqual(pFull.Manifest().Entries, p2.Manifest().Entries) {
		t.Fatal("resumed manifest entries differ")
	}
	if !reflect.DeepEqual(pFull.Manifest().NonRandomAccess, p2.Manifest().NonRandomAccess) {
		t.Fatal("resumed non-random-access intervals differ")
	}
}

func TestBuildGraphRejectsBadDependencies(t *testing.T) {
	cyc := &packet.Session{Tracks: []packet.TrackDesc{
		{SSRC: 1, ClockRate: 90000, Kind: "video", DependsOn: []uint32{2}},
		{SSRC: 2, ClockRate: 48000, Kind: "audio", DependsOn: []uint32{1}},
	}}
	if _, err := repair.TrackMetas(cyc); err == nil {
		t.Fatal("dependency cycle not rejected")
	}
	missing := &packet.Session{Tracks: []packet.TrackDesc{
		{SSRC: 1, ClockRate: 90000, Kind: "video", DependsOn: []uint32{99}},
	}}
	if _, err := repair.TrackMetas(missing); err == nil {
		t.Fatal("missing dependency not rejected")
	}
}

func TestManifestQueryByTrackAndInterval(t *testing.T) {
	opts := testkit.Options{Seed: 3, PacketsPerTrack: 1500, GOP: 30}
	recs, _ := generateRecords(t, opts)
	var buf bytes.Buffer
	p, _ := runOnce(t, recs, 0, nil, 0, &buf)
	man := p.Manifest()

	all := man.Query(0, 0, -1)
	if len(all) != len(man.Entries) {
		t.Fatalf("unbounded query returned %d of %d entries", len(all), len(man.Entries))
	}
	vid := man.Query(1, 0, -1)
	if len(vid) == 0 || len(vid) >= len(all) {
		t.Fatalf("track query returned %d of %d entries", len(vid), len(all))
	}
	mid := vid[len(vid)/2].DecodeTimeNs
	win := man.Query(1, mid, mid+1)
	if len(win) == 0 {
		t.Fatal("interval query around a keyframe returned nothing")
	}
	for _, e := range win {
		if e.DecodeTimeNs > mid+1 || e.DecodeTimeNs+e.DurationNs < mid {
			t.Fatalf("entry %d outside queried interval", e.Index)
		}
	}
}
