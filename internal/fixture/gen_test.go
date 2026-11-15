package fixture

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"tsprobe/internal/demux"
	"tsprobe/internal/psi"
	"tsprobe/internal/report"
	"tsprobe/internal/slicer"
)

func TestGeneratedFixtureEndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "room.ts")
	opt := Options{Out: path, Programs: 3, Switches: 4, Drop: 0.01, Seed: 7}
	if err := Generate(opt); err != nil {
		t.Fatal(err)
	}
	result, err := demux.Scan(path)
	if err != nil {
		t.Fatal(err)
	}
	rep := report.Build(result)
	if rep.PacketsTotal < 120000 || rep.PacketsTotal > 200000 {
		t.Fatalf("packets_total = %d", rep.PacketsTotal)
	}
	if rep.PAT.VersionChanges < 1 || rep.PAT.VersionChanges > 2 {
		t.Fatalf("PAT changes = %d", rep.PAT.VersionChanges)
	}
	if len(rep.PMT) != 3 || len(rep.Streams) != 6 {
		t.Fatalf("pmt=%d streams=%d", len(rep.PMT), len(rep.Streams))
	}
	for _, p := range rep.PMT {
		if p.VersionChanges < 1 || p.VersionChanges > 4 || len(p.Changes) != p.VersionChanges {
			t.Fatalf("bad PMT report: %+v", p)
		}
	}
	for _, s := range rep.Streams {
		if s.PacketCount < 20000 || s.PacketCount > 60000 {
			t.Fatalf("stream %d packet_count=%d", s.PID, s.PacketCount)
		}
	}
	if rep.LostPackets < 10 || rep.LostPackets > 99 || len(rep.CCGaps) >= 10 {
		t.Fatalf("lost=%d gaps=%d", rep.LostPackets, len(rep.CCGaps))
	}
	if len(rep.PAT.Changes) != rep.PAT.VersionChanges || rep.PAT.VersionChanges == 0 || rep.PAT.Changes[0].Snapshot.Programs == nil {
		t.Fatalf("missing PAT version snapshot")
	}

	json1 := filepath.Join(dir, "scan1.json")
	json2 := filepath.Join(dir, "scan2.json")
	if err := report.WriteJSON(json1, rep); err != nil {
		t.Fatal(err)
	}
	result2, err := demux.Scan(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := report.WriteJSON(json2, report.Build(result2)); err != nil {
		t.Fatal(err)
	}
	compareFiles(t, json1, json2)

	idx, err := slicer.Build(result, slicer.Options{SegmentMS: 2000, OutDir: filepath.Join(dir, "seg")})
	if err != nil {
		t.Fatal(err)
	}
	if idx.Slices.Total < 6 || idx.Slices.Total > 10 || idx.Slices.Corrupt < 1 {
		t.Fatalf("slices total=%d corrupt=%d", idx.Slices.Total, idx.Slices.Corrupt)
	}
	for _, e := range result.Store.Events() {
		for _, s := range idx.Slices.Items {
			if s.StartPacket <= e.PacketIndex && s.EndPacket >= e.PacketIndex {
				t.Fatalf("slice %d crosses version event at %d", s.Index, e.PacketIndex)
			}
		}
	}
	source, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	pkt := make([]byte, 188)
	for _, s := range idx.Slices.Items {
		if _, err := source.ReadAt(pkt, s.StartOffset); err != nil {
			t.Fatal(err)
		}
		if pkt[1]&0x40 == 0 || len(s.PIDs) == 0 {
			t.Fatalf("slice %d does not start at PUSI/mapped PID", s.Index)
		}
	}
	_ = source.Close()
	var corruptWithRanges int
	for _, s := range idx.Slices.Items {
		if s.Corrupt && len(s.PlayableRanges) > 0 {
			corruptWithRanges++
		}
	}
	if corruptWithRanges == 0 {
		t.Fatal("no corrupt slice exposes playable ranges")
	}
	indexPath := filepath.Join(dir, "index.json")
	if err := slicer.WriteSegments(path, &idx); err != nil {
		t.Fatal(err)
	}
	if err := slicer.WriteIndex(indexPath, idx); err != nil {
		t.Fatal(err)
	}
	first := idx.Slices.Items[0]
	info, err := os.Stat(first.File)
	if err != nil || info.Size()%188 != 0 || first.WrittenPackets == 0 {
		t.Fatalf("bad slice file: %v size=%d", err, info.Size())
	}
	idxAgain, _ := slicer.Build(result, slicer.Options{SegmentMS: 2000, OutDir: filepath.Join(dir, "seg")})
	if err := slicer.WriteSegments(path, &idxAgain); err != nil {
		t.Fatal(err)
	}
	if err := slicer.WriteIndex(indexPath+".again", idxAgain); err != nil {
		t.Fatal(err)
	}
	compareFiles(t, indexPath, indexPath+".again")

	path2 := filepath.Join(dir, "room2.ts")
	opt.Out = path2
	if err := Generate(opt); err != nil {
		t.Fatal(err)
	}
	if fileHash(t, path) != fileHash(t, path2) {
		t.Fatal("generator output is not deterministic")
	}
}

func TestPSIStateCurrentNextAndStale(t *testing.T) {
	programs := defaultPrograms(1)
	store := psi.NewStore()
	feedPAT(store, buildPAT(0, true, programs), 10)
	feedPAT(store, buildPAT(0, true, programs), 20)
	if info := store.PATInfo(); info.VersionChanges != 0 || info.LastVersion != 0 {
		t.Fatalf("duplicate version counted: %+v", info)
	}
	feedPAT(store, buildPAT(1, false, programs), 30)
	if store.IgnoredNext() != 1 || store.PATInfo().VersionChanges != 0 {
		t.Fatalf("current_next=0 was applied")
	}
	feedPAT(store, buildPAT(1, true, programs), 40)
	info := store.PATInfo()
	if info.VersionChanges != 1 || info.LastVersion != 1 || len(store.Events()) != 1 {
		t.Fatalf("version change missing: %+v events=%d", info, len(store.Events()))
	}
	bad := buildPAT(2, true, programs)
	bad[len(bad)-1] ^= 0xff
	store.HandlePacket(0, append([]byte{0}, bad...), true, 50, 50*188)
	if !store.PATInfo().Stale || store.CRCErrors() != 1 {
		t.Fatalf("CRC did not mark stale")
	}
	feedPAT(store, buildPAT(1, true, programs), 60)
	if store.PATInfo().Stale {
		t.Fatal("valid repeat did not clear stale")
	}
}

func TestPMTStateAndProgramReplacement(t *testing.T) {
	programs := defaultPrograms(2)
	store := psi.NewStore()
	feedPAT(store, buildPAT(0, true, programs), 10)
	feedPMT(store, programs[0].pmtPID, buildPMT(programs[0], 0, true), 20)
	feedPMT(store, programs[1].pmtPID, buildPMT(programs[1], 0, true), 30)
	if _, ok := store.Elementary(programs[0].videoPID); !ok {
		t.Fatal("initial PMT mapping missing")
	}
	programs[0].audioType = 0x03
	feedPMT(store, programs[0].pmtPID, buildPMT(programs[0], 1, true), 40)
	if info := pmtInfo(store, programs[0].pmtPID); info.VersionChanges != 1 || info.LastVersion != 1 {
		t.Fatalf("PMT version not counted: %+v", info)
	}
	if ref, ok := store.Elementary(programs[0].audioPID); !ok || ref.StreamType != 0x03 {
		t.Fatalf("PMT mapping not updated: %+v", ref)
	}
	bad := buildPMT(programs[0], 2, true)
	bad[len(bad)-1] ^= 0xff
	store.HandlePacket(programs[0].pmtPID, append([]byte{0}, bad...), true, 50, 50*188)
	if !pmtInfo(store, programs[0].pmtPID).Stale || store.CRCErrors() != 1 {
		t.Fatal("bad PMT did not fall back as stale")
	}
	feedPMT(store, programs[0].pmtPID, buildPMT(programs[0], 1, true), 60)
	if pmtInfo(store, programs[0].pmtPID).Stale {
		t.Fatal("valid PMT repeat did not clear stale")
	}

	oldPID := programs[0].pmtPID
	programs[0].pmtPID = 0x1020
	programs[0].videoPID = 0x0121
	programs[0].audioPID = 0x0122
	feedPAT(store, buildPAT(1, true, programs), 70)
	feedPMT(store, programs[0].pmtPID, buildPMT(programs[0], 0, true), 80)
	if pmtInfo(store, oldPID).Active {
		t.Fatal("replaced PMT still active")
	}
	if info := pmtInfo(store, programs[0].pmtPID); !info.Active {
		t.Fatal("replacement PMT is not active")
	}
	if ref, ok := store.Elementary(programs[0].videoPID); !ok || ref.ProgramNumber != 1 {
		t.Fatalf("replacement elementary PID missing: %+v", ref)
	}
}

func TestMalformedInputResync(t *testing.T) {
	dir := t.TempDir()
	clean := filepath.Join(dir, "clean.ts")
	if err := Generate(Options{Out: clean, Programs: 2, Switches: 0, Seed: 3}); err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(clean)
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 188*3000)
	if _, err := io.ReadFull(src, payload); err != nil {
		t.Fatal(err)
	}
	_ = src.Close()
	var bad bytes.Buffer
	bad.Write(bytes.Repeat([]byte{0x99}, 37))
	bad.Write(payload)
	bad.Write(make([]byte, 188))
	bad.Write(bytes.Repeat([]byte{0x47}, 37))
	badPath := filepath.Join(dir, "bad.ts")
	if err := os.WriteFile(badPath, bad.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := demux.Scan(badPath)
	if err != nil {
		t.Fatal(err)
	}
	rep := report.Build(result)
	if rep.PacketsTotal == 0 || rep.PAT.FirstVersion == nil {
		t.Fatal("malformed stream was not recovered")
	}
	a := rep.Anomalies
	if a.SyncLossEvents == 0 || a.ZeroFillSegments == 0 || !a.NonMultipleOf188 || a.TrailingBytes == 0 {
		t.Fatalf("anomaly counters incomplete: %+v", a)
	}
}

func feedPAT(store *psi.Store, section []byte, idx uint64) {
	store.HandlePacket(0, append([]byte{0}, section...), true, idx, int64(idx*188))
}

func feedPMT(store *psi.Store, pid uint16, section []byte, idx uint64) {
	store.HandlePacket(pid, append([]byte{0}, section...), true, idx, int64(idx*188))
}

func pmtInfo(store *psi.Store, pid uint16) psi.TableInfo {
	for _, info := range store.PMTInfos() {
		if info.PID == pid {
			return info
		}
	}
	return psi.TableInfo{}
}

func compareFiles(t *testing.T, a, b string) {
	t.Helper()
	ab, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ab, bb) {
		t.Fatalf("files differ: %s %s", a, b)
	}
	var doc map[string]any
	if err := json.Unmarshal(ab, &doc); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
}

func fileHash(t *testing.T, path string) [32]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
