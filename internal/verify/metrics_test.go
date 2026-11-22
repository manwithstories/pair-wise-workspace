package verify

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"mediabridge/internal/adapter"
	mp4adapter "mediabridge/internal/adapter/mp4"
	"mediabridge/internal/core"
	"mediabridge/internal/fixture"
	"mediabridge/internal/maptrack"
	"mediabridge/internal/media"
)

func remuxed(t *testing.T) (string, *core.Provenance) {
	t.Helper()
	dir := t.TempDir()
	rep, err := fixture.GenerateMP4(dir, "clip.mp4", fixture.DefaultMP4Options(41))
	if err != nil {
		t.Fatal(err)
	}
	asset, err := (&mp4adapter.Reader{}).Parse(
		adapter.Source{AssetID: "A1", Path: rep.Path}, adapter.Options{KeepPayload: true})
	if err != nil {
		t.Fatal(err)
	}
	plan := &maptrack.Plan{Assignments: []maptrack.Assignment{
		{Slot: "video", TrackIndex: 0, TrackID: 1, Kind: media.KindVideo},
		{Slot: "audio", TrackIndex: 1, TrackID: 2, Kind: media.KindAudio},
	}}
	prov := core.NewProvenance("A1", asset.SourceSHA, "mp4", 1)
	prov.AddTrack("video", asset.Tracks[0])
	prov.AddTrack("audio", asset.Tracks[1])
	out := filepath.Join(dir, "out.mbx")
	if _, err := core.Remux(core.RemuxInput{Asset: asset, Plan: plan, Provenance: prov}, out); err != nil {
		t.Fatal(err)
	}
	return out, prov
}

func TestVerifyPassesCleanRun(t *testing.T) {
	out, prov := remuxed(t)
	res, err := Verify(Input{
		OutputPath:      out,
		Provenance:      prov,
		SourceDelivered: 600,
		SourceLost:      6,
		Latency:         12 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	m := res.Metrics
	if m.CopiedPacketHashMatchPct != 100 {
		t.Fatalf("hash match = %f", m.CopiedPacketHashMatchPct)
	}
	if m.ProvenanceCoveragePct != 100 {
		t.Fatalf("coverage = %f", m.ProvenanceCoveragePct)
	}
	if m.SyncErrorMS > MaxSyncErrorMS {
		t.Fatalf("sync error = %f", m.SyncErrorMS)
	}
	if m.PacketRecoveryRate < MinRecoveryRate {
		t.Fatalf("recovery = %f", m.PacketRecoveryRate)
	}
	if m.P95LatencyMS > MaxP95LatencyMS {
		t.Fatalf("latency = %f", m.P95LatencyMS)
	}
	if !res.Passed() {
		t.Fatalf("expected pass, failures: %v", res.Failed)
	}
}

func TestVerifyDetectsTamper(t *testing.T) {
	out, prov := remuxed(t)
	// Corrupt one payload byte in the output.
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xFF
	if err := os.WriteFile(out, data, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Verify(Input{OutputPath: out, Provenance: prov})
	if err != nil {
		t.Fatal(err)
	}
	if res.Metrics.CopiedPacketHashMatchPct >= 100 {
		t.Fatalf("tamper not detected: %f", res.Metrics.CopiedPacketHashMatchPct)
	}
	if res.Passed() {
		t.Fatal("tampered output should fail acceptance")
	}
}

func TestVerifyDetectsLostPackets(t *testing.T) {
	out, prov := remuxed(t)
	res, err := Verify(Input{
		OutputPath:      out,
		Provenance:      prov,
		SourceDelivered: 90,
		SourceLost:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Metrics.PacketRecoveryRate >= MinRecoveryRate {
		t.Fatalf("recovery = %f should be below threshold", res.Metrics.PacketRecoveryRate)
	}
	if res.Passed() {
		t.Fatal("excess loss should fail acceptance")
	}
}

func TestVerifyRejectsAssetMismatch(t *testing.T) {
	out, prov := remuxed(t)
	prov.AssetID = "OTHER"
	_, err := Verify(Input{OutputPath: out, Provenance: prov})
	if err == nil {
		t.Fatal("asset id mismatch should error")
	}
}

func TestSyncErrorZeroForAlignedTracks(t *testing.T) {
	hdr := &core.OutputHeader{AssetID: "x"}
	packets := []core.OutputPacket{
		{Slot: "video", PTSNS: 0},
		{Slot: "video", PTSNS: 40_000_000},
		{Slot: "audio", PTSNS: 0},
		{Slot: "audio", PTSNS: 21_000_000},
	}
	if got := SyncError(hdr, packets); got != 0 {
		t.Fatalf("sync = %f", got)
	}
}

func TestSyncErrorMeasuresSkew(t *testing.T) {
	hdr := &core.OutputHeader{AssetID: "x"}
	packets := []core.OutputPacket{
		{Slot: "video", PTSNS: 0},
		{Slot: "audio", PTSNS: 2_000_000}, // 2ms late
	}
	if got := SyncError(hdr, packets); got != 2 {
		t.Fatalf("sync = %f want 2", got)
	}
}

func TestPacketHashMatches(t *testing.T) {
	out, prov := remuxed(t)
	matched, total, err := PacketHashMatches(out, prov)
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 || matched != total {
		t.Fatalf("matched %d / %d", matched, total)
	}
}

// TestDuplicateDetection proves the resumed_duplicate_packets metric is not a
// yes-man: it fires when the same source packet appears twice in the output.
func TestDuplicateDetection(t *testing.T) {
	packets := []core.OutputPacket{
		{Slot: "video", SourceTrack: 1, SourceIndex: 0, SourceSize: 3, Payload: []byte{1, 2, 3}},
		{Slot: "video", SourceTrack: 1, SourceIndex: 1, SourceSize: 3, Payload: []byte{4, 5, 6}},
		{Slot: "video", SourceTrack: 1, SourceIndex: 0, SourceSize: 3, Payload: []byte{1, 2, 3}}, // double write
	}
	if got := countDuplicates(packets); got != 1 {
		t.Fatalf("duplicates = %d want 1", got)
	}
}
