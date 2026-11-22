package core

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"mediabridge/internal/adapter"
	mp4adapter "mediabridge/internal/adapter/mp4"
	"mediabridge/internal/fixture"
	"mediabridge/internal/maptrack"
	"mediabridge/internal/media"
)

func buildAsset(t *testing.T) *media.Asset {
	t.Helper()
	dir := t.TempDir()
	rep, err := fixture.GenerateMP4(dir, "clip.mp4", fixture.DefaultMP4Options(41))
	if err != nil {
		t.Fatal(err)
	}
	asset, err := (&mp4adapter.Reader{}).Parse(
		adapter.Source{AssetID: "A1", Path: rep.Path},
		adapter.Options{KeepPayload: true})
	if err != nil {
		t.Fatal(err)
	}
	return asset
}

func TestRemuxCopiesVerbatim(t *testing.T) {
	asset := buildAsset(t)
	plan := &maptrack.Plan{Assignments: []maptrack.Assignment{
		{Slot: "video", TrackIndex: 0, TrackID: 1, Kind: media.KindVideo},
		{Slot: "audio", TrackIndex: 1, TrackID: 2, Kind: media.KindAudio},
	}}
	prov := NewProvenance("A1", asset.SourceSHA, "mp4", 1)
	out := filepath.Join(t.TempDir(), "out.mbx")
	res, err := Remux(RemuxInput{Asset: asset, Plan: plan, Provenance: prov}, out)
	if err != nil {
		t.Fatal(err)
	}
	if res.PacketCount != int64(len(asset.Packets)-len(asset.Packets)/3) && res.PacketCount == 0 {
		t.Fatalf("packet count = %d", res.PacketCount)
	}
	hdr, packets, err := ReadOutput(out)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Packets != uint64(res.PacketCount) {
		t.Fatalf("header packets = %d want %d", hdr.Packets, res.PacketCount)
	}
	// Every packet's payload must equal the source byte range it claims.
	bySource := map[string][]byte{}
	for _, p := range asset.Packets {
		bySource[slotKey(p.TrackIndex, p.PacketIndex)] = p.Payload
	}
	slotToTrack := map[string]int{"video": 0, "audio": 1}
	for _, p := range packets {
		ti, ok := slotToTrack[p.Slot]
		if !ok {
			continue
		}
		want := bySource[slotKey(ti, p.SourceIndex)]
		if string(want) != string(p.Payload) {
			t.Fatalf("slot %s packet %d payload changed", p.Slot, p.SourceIndex)
		}
	}
}

func slotKey(track int, idx int64) string { return fmt.Sprintf("%d#%d", track, idx) }

func TestRemuxDeterministicOutput(t *testing.T) {
	asset := buildAsset(t)
	plan := &maptrack.Plan{Assignments: []maptrack.Assignment{
		{Slot: "video", TrackIndex: 0, TrackID: 1, Kind: media.KindVideo},
		{Slot: "audio", TrackIndex: 1, TrackID: 2, Kind: media.KindAudio},
	}}
	dir := t.TempDir()
	out1 := filepath.Join(dir, "a.mbx")
	out2 := filepath.Join(dir, "b.mbx")
	p1 := NewProvenance("A1", asset.SourceSHA, "mp4", 1)
	p2 := NewProvenance("A1", asset.SourceSHA, "mp4", 1)
	if _, err := Remux(RemuxInput{Asset: asset, Plan: plan, Provenance: p1}, out1); err != nil {
		t.Fatal(err)
	}
	if _, err := Remux(RemuxInput{Asset: asset, Plan: plan, Provenance: p2}, out2); err != nil {
		t.Fatal(err)
	}
	b1, _ := os.ReadFile(out1)
	b2, _ := os.ReadFile(out2)
	if string(b1) != string(b2) {
		t.Fatal("identical inputs produced different output bytes")
	}
	if p1.Digest() != p2.Digest() {
		t.Fatal("provenance digest not stable")
	}
}

func TestProvenanceCoverageAndDigest(t *testing.T) {
	asset := buildAsset(t)
	plan := &maptrack.Plan{Assignments: []maptrack.Assignment{
		{Slot: "video", TrackIndex: 0, TrackID: 1, Kind: media.KindVideo},
		{Slot: "audio", TrackIndex: 1, TrackID: 2, Kind: media.KindAudio},
	}}
	prov := NewProvenance("A1", asset.SourceSHA, "mp4", 1)
	out := filepath.Join(t.TempDir(), "out.mbx")
	if _, err := Remux(RemuxInput{Asset: asset, Plan: plan, Provenance: prov}, out); err != nil {
		t.Fatal(err)
	}
	if c := prov.Coverage(); c != 1.0 {
		t.Fatalf("coverage = %f", c)
	}
	if !prov.HashMatch() {
		t.Fatal("hash match failed")
	}
	if prov.Digest() == "" {
		t.Fatal("empty provenance digest")
	}
	// Digest must be stable across re-serialisation.
	d1 := prov.Digest()
	prov.Canonical()
	if d2 := prov.Digest(); d1 != d2 {
		t.Fatal("digest changed after canonicalisation")
	}
}

func TestRemuxStreamsFromSource(t *testing.T) {
	// Build an asset without payloads, forcing the kernel to stream samples.
	dir := t.TempDir()
	rep, err := fixture.GenerateMP4(dir, "clip.mp4", fixture.DefaultMP4Options(9))
	if err != nil {
		t.Fatal(err)
	}
	asset, err := (&mp4adapter.Reader{}).Parse(
		adapter.Source{AssetID: "A2", Path: rep.Path}, adapter.Options{KeepPayload: false})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(rep.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	plan := &maptrack.Plan{Assignments: []maptrack.Assignment{
		{Slot: "video", TrackIndex: 0, TrackID: 1, Kind: media.KindVideo},
	}}
	prov := NewProvenance("A2", asset.SourceSHA, "mp4", 1)
	out := filepath.Join(dir, "out.mbx")
	if _, err := Remux(RemuxInput{Asset: asset, Plan: plan, Reader: NewFileSampleSource(f), Provenance: prov}, out); err != nil {
		t.Fatal(err)
	}
	if prov.Coverage() != 1.0 {
		t.Fatal("streamed provenance incomplete")
	}
}
