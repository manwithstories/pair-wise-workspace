package scan_test

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"aircheck/internal/clock"
	"aircheck/internal/scan"
	"aircheck/internal/fault"
	"aircheck/internal/remux"
)

// writeSample renders a minimal two-track MP4 via the remux writer.
func writeSample(t *testing.T, dir string, truncateTo int) string {
	t.Helper()
	payload := make([]byte, 60000)
	for i := range payload {
		payload[i] = byte(i)
	}
	mk := func(id uint32, media string, ts uint32, n int, delta int64, keysEvery int) remux.TrackData {
		td := remux.TrackData{ID: id, MediaType: media, Timescale: ts}
		off := int64(0)
		if id == 2 {
			off = 20000
		}
		for i := 0; i < n; i++ {
			td.Packets = append(td.Packets, scan.Packet{
				TrackID: id, Index: uint32(i + 1), Offset: off, Size: 400,
				DTS: int64(i) * delta, PTS: int64(i) * delta,
				Keyframe: media != "video" || i%keysEvery == 0,
			})
			off += 400
		}
		return td
	}
	var buf bytes.Buffer
	err := remux.WriteContainer(&buf, bytes.NewReader(payload), []remux.TrackData{
		mk(1, "video", 12800, 50, 512, 10),
		mk(2, "audio", 48000, 94, 1024, 0),
	})
	if err != nil {
		t.Fatalf("write container: %v", err)
	}
	data := buf.Bytes()
	if truncateTo > 0 && truncateTo < len(data) {
		data = data[:truncateTo]
	}
	path := filepath.Join(dir, "sample.mp4")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func parseFile(t *testing.T, path string) (*scan.ContainerInfo, int64) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	ci, err := scan.Parse(f, st.Size())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return ci, st.Size()
}

func TestParseNormalContainer(t *testing.T) {
	path := writeSample(t, t.TempDir(), 0)
	ci, size := parseFile(t, path)
	if ci.DamageOffset != -1 {
		t.Fatalf("unexpected damage at %d", ci.DamageOffset)
	}
	if len(ci.Tracks) != 2 {
		t.Fatalf("want 2 tracks, got %d", len(ci.Tracks))
	}
	v, a := ci.Tracks[0], ci.Tracks[1]
	if v.MediaType != "video" || v.Timescale != 12800 {
		t.Errorf("video track: type=%s timescale=%d", v.MediaType, v.Timescale)
	}
	if a.MediaType != "audio" || a.Timescale != 48000 {
		t.Errorf("audio track: type=%s timescale=%d", a.MediaType, a.Timescale)
	}
	pkts, fl := scan.BuildPackets(v, size)
	if len(fl) != 0 || len(pkts) != 50 {
		t.Fatalf("video packets=%d faults=%v", len(pkts), fl)
	}
	keys := 0
	for _, p := range pkts {
		if p.Keyframe {
			keys++
		}
	}
	if keys != 5 {
		t.Errorf("want 5 keyframes, got %d", keys)
	}
	// Unified timeline: first audio and video PTS must align at 0.
	apkts, _ := scan.BuildPackets(a, size)
	if apkts[0].PTS != 0 || pkts[0].PTS != 0 {
		t.Errorf("A/V first PTS misaligned: v=%d a=%d", pkts[0].PTS, apkts[0].PTS)
	}
}

func TestParseTruncatedContainer(t *testing.T) {
	dir := t.TempDir()
	full := writeSample(t, dir, 0)
	st, _ := os.Stat(full)
	path := writeSample(t, dir, int(st.Size())*3/5)
	ci, size := parseFile(t, path)
	if ci.DamageOffset < 0 {
		t.Fatal("truncated file must report a damage offset")
	}
	if uf := ci.Faults.Unrecoverable(); uf != nil {
		t.Fatalf("tail truncation must be recoverable: %v", uf)
	}
	// The video track sits first in mdat and survives whole; the audio
	// track is cut and only its intact prefix may be retained.
	vpkts, _ := scan.BuildPackets(ci.Tracks[0], size)
	if len(vpkts) != 50 {
		t.Fatalf("video track should survive whole, got %d packets", len(vpkts))
	}
	apkts, fl := scan.BuildPackets(ci.Tracks[1], size)
	if len(apkts) == 0 || len(apkts) >= 94 {
		t.Fatalf("want retained audio prefix, got %d packets", len(apkts))
	}
	if uf := fl.Unrecoverable(); uf != nil {
		t.Fatalf("prefix retention must stay recoverable: %v", uf)
	}
}

func TestOversizedBoxRejectedBeforeAllocation(t *testing.T) {
	// A box header claiming 1<<40 bytes must be rejected, not allocated.
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, uint32(8))
	buf.WriteString("free")
	binary.Write(&buf, binary.BigEndian, uint32(1<<30))
	buf.WriteString("moov")
	ci, err := scan.Parse(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ci.DamageOffset != 8 {
		t.Fatalf("want damage offset 8, got %d", ci.DamageOffset)
	}
	if len(ci.Faults.ByKind(fault.KindBoxOversized)) != 1 {
		t.Fatalf("want oversized-box fault, got %v", ci.Faults)
	}
}

func TestTimestampUnwrapAndAnomalies(t *testing.T) {
	u, err := clock.NewUnwrapper(32)
	if err != nil {
		t.Fatal(err)
	}
	seq := []uint32{0xFFFFFFFE, 0xFFFFFFFF, 0, 1, 2}
	var got []int64
	for _, raw := range seq {
		got = append(got, u.Next(uint64(raw)))
	}
	for i := 1; i < len(got); i++ {
		if got[i] != got[i-1]+1 {
			t.Fatalf("unwrap broke at %d: %v", i, got)
		}
	}

	ts := &clock.TrackState{TrackID: 1, Timescale: 90000}
	if _, err := ts.Observe(100, 0); err != nil {
		t.Fatal(err)
	}
	ts.Observe(100, 8)  // duplicate
	ts.Observe(50, 16)  // regression
	if len(ts.Faults.ByKind(fault.KindTSDuplicate)) != 1 {
		t.Error("duplicate not detected")
	}
	if len(ts.Faults.ByKind(fault.KindTSRegression)) != 1 {
		t.Error("regression not detected")
	}
}

func TestKeyframeGapOffsets(t *testing.T) {
	var pkts []scan.Packet
	for i := 0; i < 30; i++ {
		pkts = append(pkts, scan.Packet{
			TrackID: 1, Index: uint32(i + 1), Offset: int64(1000 + i*100),
			DTS: int64(i) * 3600, PTS: int64(i) * 3600,
			Keyframe: i == 0,
		})
	}
	gaps := scan.KeyframeGaps(pkts, 12)
	if len(gaps) != 1 {
		t.Fatalf("want 1 gap, got %d", len(gaps))
	}
	if gaps[0].StartOffset != 1000 || gaps[0].Length != 29 {
		t.Errorf("gap: offset=%d len=%d", gaps[0].StartOffset, gaps[0].Length)
	}
	if gaps[0].StartTime != 0 {
		t.Errorf("gap start time on unified base: %d", gaps[0].StartTime)
	}
	_ = clock.Base
}
