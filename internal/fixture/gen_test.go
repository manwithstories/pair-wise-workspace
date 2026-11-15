package fixture_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"tsprobe/internal/fixture"
	"tsprobe/internal/program"
	"tsprobe/internal/psi"
	"tsprobe/internal/report"
	"tsprobe/internal/segment"
	"tsprobe/internal/tsdemux"
)

// runE2E generates the acceptance fixture and runs the full pipeline.
func runE2E(t *testing.T) (*report.Report, *program.State, *segment.Manifest) {
	t.Helper()
	dir := t.TempDir()
	ts := filepath.Join(dir, "mux.ts")
	sum, err := fixture.Generate(fixture.Options{
		Out: ts, Programs: 2, Seconds: 30,
		Drop: 0.01, CCJumps: 7, PATVersions: 3, PMTVersions: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(ts)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dm := tsdemux.New(f)
	cc := tsdemux.NewCCTracker()
	col := report.NewCollector()
	st := program.NewState()
	seg := segment.New(filepath.Join(dir, "out"), 1, 4000, st)
	os.MkdirAll(filepath.Join(dir, "out"), 0o755)
	patAsm := &psi.Assembler{}
	pmtAsms := map[uint16]*psi.Assembler{}
	var crc int64
	for {
		p, err := dm.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		cc.Observe(p)
		col.Observe(p)
		changed := false
		if !p.TEI && !p.Scrambled && p.HasPayload {
			if p.PID == 0 {
				before := len(st.Events)
				patAsm.Write(p.PUSI, p.Payload, func(s []byte) { st.FeedPAT(s, p.Index) })
				changed = len(st.Events) > before
			} else if _, ok := st.PMTOwner[p.PID]; ok {
				asm := pmtAsms[p.PID]
				if asm == nil {
					asm = &psi.Assembler{}
					pmtAsms[p.PID] = asm
				}
				before := len(st.Events)
				pid := p.PID
				asm.Write(p.PUSI, p.Payload, func(s []byte) { st.FeedPMT(pid, s, p.Index) })
				changed = len(st.Events) > before
			}
		}
		if err := seg.Observe(p, changed); err != nil {
			t.Fatal(err)
		}
	}
	crc = patAsm.CRCErrors
	for _, a := range pmtAsms {
		crc += a.CRCErrors
	}
	m, err := seg.Finish(ts)
	if err != nil {
		t.Fatal(err)
	}
	rep := report.Build(ts, dm, cc, col, st, crc)

	// Cross-check against the generator summary.
	if rep.PacketCount != sum.Packets {
		t.Fatalf("packet_count %d != generated %d", rep.PacketCount, sum.Packets)
	}
	if rep.CCJumps != int64(sum.CCJumps) || rep.LostPackets != sum.ExpectedLost {
		t.Fatalf("cc %d/%d != %d/%d", rep.CCJumps, rep.LostPackets, sum.CCJumps, sum.ExpectedLost)
	}
	if rep.DupPackets != sum.Dups {
		t.Fatalf("dup %d != %d", rep.DupPackets, sum.Dups)
	}
	return rep, st, m
}

func TestAcceptanceFixture(t *testing.T) {
	rep, st, m := runE2E(t)
	if rep.PacketCount < 90_000 || rep.PacketCount > 130_000 {
		t.Fatalf("packet_count %d", rep.PacketCount)
	}
	if rep.PATVersionChanges != 2 {
		t.Fatalf("pat changes %d", rep.PATVersionChanges)
	}
	if rep.PMTVersionChanges["1"] != 3 || rep.PMTVersionChanges["2"] != 3 {
		t.Fatalf("pmt changes %v", rep.PMTVersionChanges)
	}
	if n := len(rep.PIDMap); n < 6 || n > 10 {
		t.Fatalf("pid_map %d entries", n)
	}
	if rep.CRCErrors != 0 || rep.SyncLossEvents != 0 {
		t.Fatalf("crc %d sync %d", rep.CRCErrors, rep.SyncLossEvents)
	}
	var total int64
	for _, s := range rep.Streams {
		total += s.Packets
	}
	if total != rep.PacketCount {
		t.Fatalf("stream sum %d != %d", total, rep.PacketCount)
	}
	if len(st.Validate()) != 0 {
		t.Fatalf("validate %v", st.Validate())
	}
	if len(m.Segments) != 8 {
		t.Fatalf("segments %d", len(m.Segments))
	}
	for _, s := range m.Segments {
		if s.Packets == 0 || len(s.Streams) == 0 {
			t.Fatalf("segment %+v", s)
		}
	}
}
