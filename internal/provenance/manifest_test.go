package provenance

import (
	"path/filepath"
	"testing"
)

func TestWriteLoadQuery(t *testing.T) {
	m := NewManifest()
	m.InputSHA256 = "in"
	m.ConfigSHA256 = "cfg"
	m.OutputSHA256 = "out"
	m.Params["window"] = 512
	m.TimeBase["4369"] = "rtcp-sr"
	m.Entries = []Entry{
		{Index: 0, Fragment: 1, TrackID: 1, SSRC: 4369, Kind: "keyframe",
			DecodeTimeNs: 0, DurationNs: 2_000_000_000, SrcFirstExtSeq: 0, SrcLastExtSeq: 59},
		{Index: 1, Fragment: 1, TrackID: 2, SSRC: 8738, Kind: "sample-boundary",
			DecodeTimeNs: 0, DurationNs: 2_000_000_000, SrcFirstExtSeq: 0, SrcLastExtSeq: 99},
		{Index: 2, Fragment: 2, TrackID: 1, SSRC: 4369, Kind: "keyframe",
			DecodeTimeNs: 2_000_000_000, DurationNs: 2_000_000_000,
			SrcFirstExtSeq: 60, SrcLastExtSeq: 119, Repair: "loss-gap-before"},
	}
	m.NonRandomAccess = []Interval{{TrackID: 1, SSRC: 4369, FromNs: 100, ToNs: 200, Reason: "loss"}}
	m.Errors = []ErrorEntry{{Kind: "loss", SSRC: 4369, ExtSeq: 4812}}

	p := filepath.Join(t.TempDir(), "manifest.json")
	if err := Write(p, m); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.OutputSHA256 != "out" || len(got.Entries) != 3 {
		t.Fatalf("%+v", got)
	}
	if q := got.Query(1, 0, 1_000_000_000); len(q) != 1 || q[0].Index != 0 {
		t.Fatalf("query track1: %+v", q)
	}
	if q := got.Query(0, 2_500_000_000, -1); len(q) != 1 || q[0].Index != 2 {
		t.Fatalf("query all late: %+v", q)
	}
	if q := got.Query(2, 0, -1); len(q) != 1 {
		t.Fatalf("query track2: %+v", q)
	}
}
