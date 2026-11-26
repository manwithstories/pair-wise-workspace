package fixture

import (
	"os"
	"path/filepath"
	"testing"

	"relaycheck/internal/demux"
)

func TestGenerateAndParse(t *testing.T) {
	out := t.TempDir()
	m, err := Generate(Options{
		Out: out, Seed: 17, Segments: 64,
		Faults: []string{"gap", "rollback", "duplicate", "truncate", "loss", "map-conflict"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Faults) != 6 {
		t.Fatalf("faults=%v", m.Faults)
	}
	// init parses with both tracks
	initData, err := os.ReadFile(filepath.Join(out, "clean", "init.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	info, fault := demux.ParseInit(initData)
	if fault != nil {
		t.Fatal(fault)
	}
	if len(info.Tracks) != 2 {
		t.Fatalf("tracks=%+v", info.Tracks)
	}
	v, a := info.Tracks[0], info.Tracks[1]
	if v.ID != VideoTrackID || v.Codec != "avc1" || v.Timescale != VideoTimescale || v.Handler != "vide" {
		t.Fatalf("video track=%+v", v)
	}
	if a.ID != AudioTrackID || a.Codec != "mp4a" || a.Timescale != AudioTimescale ||
		a.Language != "eng" || a.Channels != 2 {
		t.Fatalf("audio track=%+v", a)
	}
	// a clean segment parses and extracts
	segData, err := os.ReadFile(filepath.Join(out, "clean", SegmentName(2)))
	if err != nil {
		t.Fatal(err)
	}
	seg, fault := demux.ParseSegment(segData, 2)
	if fault != nil {
		t.Fatal(fault)
	}
	if seg.Sequence != 2 || len(seg.Traf) != 2 {
		t.Fatalf("seg=%+v", seg)
	}
	all, fault := seg.ExtractAll(2)
	if fault != nil {
		t.Fatal(fault)
	}
	if len(all[0]) != VideoPerSeg || len(all[1]) != AudioPerSeg {
		t.Fatalf("samples=%d/%d", len(all[0]), len(all[1]))
	}
	// every injected fault is observable in the faulty stream
	for _, spec := range m.Faults {
		name := SegmentName(spec.Seq)
		path := filepath.Join(out, "faulty", name)
		data, err := os.ReadFile(path)
		switch spec.Type {
		case "loss":
			if !os.IsNotExist(err) {
				t.Fatalf("loss segment %d should be missing", spec.Seq)
			}
		case "truncate":
			seg, fault := demux.ParseSegment(data, spec.Seq)
			if seg != nil || fault == nil || fault.Offset < 0 {
				t.Fatalf("truncate %d: %v", spec.Seq, fault)
			}
		case "gap":
			seg, _ := demux.ParseSegment(data, spec.Seq)
			if seg == nil || seg.Sequence != spec.Seq+1 {
				t.Fatalf("gap %d: %+v", spec.Seq, seg)
			}
		case "rollback":
			seg, _ := demux.ParseSegment(data, spec.Seq)
			if seg == nil || seg.Traf[0].TFDT != uint64(spec.Seq-6)*SegDurVideo {
				t.Fatalf("rollback %d: %+v", spec.Seq, seg.Traf[0])
			}
		case "map-conflict":
			seg, _ := demux.ParseSegment(data, spec.Seq)
			if seg == nil || seg.Traf[1].TrackID != 99 {
				t.Fatalf("map-conflict %d: %+v", spec.Seq, seg.Traf[1])
			}
		case "duplicate":
			dup := filepath.Join(out, "faulty", "seg_"+name[4:10]+"_dup1.m4s")
			dupData, err := os.ReadFile(dup)
			if err != nil {
				t.Fatal(err)
			}
			if DigestFile(dupData) != DigestFile(data) {
				t.Fatalf("duplicate %d digest mismatch", spec.Seq)
			}
		}
	}
	// faults must be recoverable: never at the tail
	for _, spec := range m.Faults {
		if spec.Seq >= uint32(m.Segments)-1 {
			t.Fatalf("fault at tail: %+v", spec)
		}
	}
}
