package manifest

import (
	"bytes"
	"os"
	"testing"

	mp4 "github.com/abema/go-mp4"

	"mediarchive/internal/clock"
	"mediarchive/internal/event"
	"mediarchive/internal/index"
	"mediarchive/internal/repair"
	"mediarchive/internal/safepath"
	"mediarchive/internal/segment"
)

func testCfg() event.CodecConfig {
	return event.CodecConfig{Codec: event.CodecOpus, ClockRate: 48000, Channels: 2}
}

func pkts(start, n uint64, step clock.Time) []*event.Packet {
	out := make([]*event.Packet, 0, n)
	for i := uint64(0); i < n; i++ {
		out = append(out, &event.Packet{
			SeqExt: start + i, TSExt: (start + i) * 960,
			MediaTime: clock.Time(start+i) * step, Duration: step,
			Payload: []byte{19 << 3, byte(i), byte(i >> 8)},
		})
	}
	return out
}

// Round-trip: packets -> repair -> index -> fMP4 segments -> manifest,
// then validate the manifest and re-parse the init segment with go-mp4.
func TestSegmentManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	res := repair.Repair(pkts(0, 250, 1800), nil) // 5 s of 20 ms packets
	idx := index.Build(res.Packets, segment.DefaultDuration)
	infos, err := segment.Write(dir, res.Packets, testCfg(), segment.DefaultDuration)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 3 { // 2 s + 2 s + 1 s
		t.Fatalf("segments=%d want 3", len(infos))
	}
	m := Build(testCfg(), infos, res.Gaps, nil, 0, res.Degraded, "00")
	align, problems := m.Validate(idx, 1800)
	if len(problems) != 0 {
		t.Fatalf("problems=%v", problems)
	}
	if align != 0 {
		t.Fatalf("align=%d want 0", align)
	}
	if err := m.Write(dir); err != nil {
		t.Fatal(err)
	}
	// The init segment must parse as valid MP4 box structure.
	initPath, err := safepath.Join(dir, "init.mp4")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(initPath)
	if err != nil {
		t.Fatal(err)
	}
	var sawStsd, sawDOps bool
	_, err = mp4.ReadBoxStructure(bytes.NewReader(data), func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(),
			mp4.BoxTypeMinf(), mp4.BoxTypeDinf(), mp4.BoxTypeDref(),
			mp4.BoxTypeStbl(), mp4.BoxTypeStsd(), mp4.BoxTypeMvex(), mp4.BoxTypeOpus():
			if h.BoxInfo.Type == mp4.BoxTypeStsd() {
				sawStsd = true
			}
			return h.Expand()
		case mp4.BoxTypeDOps():
			sawDOps = true
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawStsd || !sawDOps {
		t.Fatalf("init segment incomplete: stsd=%v dOps=%v", sawStsd, sawDOps)
	}
}

func TestValidateDetectsOverlapAndUndeclaredHole(t *testing.T) {
	m := &Manifest{
		Segments: []SegmentRef{
			{ID: 0, Start: 0, End: 18000},
			{ID: 1, Start: 9000, End: 36000},  // overlaps segment 0
			{ID: 2, Start: 72000, End: 90000}, // undeclared hole
		},
	}
	idx := &index.Index{Entries: []index.Entry{
		{Time: 0, SeqExt: 0}, {Time: 9000, SeqExt: 5}, {Time: 72000, SeqExt: 40},
	}}
	_, problems := m.Validate(idx, 1800)
	if len(problems) != 2 {
		t.Fatalf("problems=%v want 2", problems)
	}
}

func TestDeclaredGapIsNotAProblem(t *testing.T) {
	m := &Manifest{
		Segments: []SegmentRef{
			{ID: 0, Start: 0, End: 18000},
			{ID: 1, Start: 36000, End: 54000},
		},
		Gaps: []Gap{{AfterSeq: 9, Count: 10, Start: 18000, End: 36000, Source: "loss_burst", Confidence: 1}},
	}
	idx := &index.Index{Entries: []index.Entry{{Time: 0}, {Time: 36000, SeqExt: 20}}}
	align, problems := m.Validate(idx, 1800)
	if len(problems) != 0 {
		t.Fatalf("problems=%v", problems)
	}
	if align != 0 {
		t.Fatalf("align=%d", align)
	}
}

func TestUnrepairableAddsAlignmentError(t *testing.T) {
	m := &Manifest{
		Segments:     []SegmentRef{{ID: 0, Start: 0, End: 18000}},
		Unrepairable: []Mark{{SeqExt: 5, MediaTime: 9000, Reason: "late_beyond_window"}},
	}
	idx := &index.Index{Entries: []index.Entry{{Time: 0}}}
	align, problems := m.Validate(idx, 1800)
	if len(problems) != 0 {
		t.Fatalf("problems=%v", problems)
	}
	if align != 1800 {
		t.Fatalf("align=%d want 1800", align)
	}
}
