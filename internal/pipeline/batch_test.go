package pipeline

import (
	"reflect"
	"testing"

	"media-check/internal/adapter"
	"media-check/internal/adapter/audio"
	"media-check/internal/adapter/frame"
	mp4adapter "media-check/internal/adapter/mp4"
	"media-check/internal/manifest"
	"media-check/internal/model"
)

func testRegistry() *adapter.Registry {
	reg := adapter.NewRegistry()
	reg.Register(frame.Adapter{})
	reg.Register(mp4adapter.Adapter{})
	reg.Register(audio.Adapter{})
	return reg
}

func frameAsset(t *testing.T, name string, startTS uint32, corruptSegs map[int]bool, withAudio bool) Asset {
	t.Helper()
	const n, seg = 60, 20
	recs := frame.Generate(frame.GenOptions{
		Count: n, StartSeq: 100, StartTS: startTS, TSDelta: 100,
		GOPSize: 10, Width: 320, Height: 240, PayloadSize: 24, Seed: 11,
		Color: model.ColorMeta{Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"},
	})
	for i := range recs {
		if corruptSegs[i/seg] {
			recs[i].Payload[0] ^= 0xFF // damage after checksum was issued
		}
	}
	pkts, err := frame.ToPackets(recs, 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	a := Asset{
		Name: name, Kind: "frames", Packets: pkts,
		StartSeq: 100, SegmentFrames: seg, TSDelta: 100,
	}
	if withAudio {
		metas := make([]audio.Meta, n/seg)
		for i := range metas {
			metas[i] = audio.Meta{Channels: 2, Layout: "stereo", SampleRate: 48000}
		}
		metas[1] = audio.Meta{Channels: 6, Layout: "5.1", SampleRate: 44100}
		a.Audio = metas
	}
	return a
}

func TestBoundaryTimestampWrap(t *testing.T) {
	p := NewPipeline(testRegistry(), NewStore())
	res := p.RunAsset(frameAsset(t, "wrap", 0xFFFFFF00, nil, false))
	if res.Faults.HasCode(model.FaultTimestampRegression) {
		t.Fatalf("wrap mishandled: %v", res.Faults)
	}
	if res.CorruptRanges != 0 {
		t.Fatalf("wrap is not corruption: %d ranges", res.CorruptRanges)
	}
	for _, e := range res.Segments {
		if e.Status != manifest.StatusOK || !e.HashMatch() {
			t.Fatalf("segment %+v", e)
		}
	}
}

func TestBoundaryHashCorruptPropagatesAndReruns(t *testing.T) {
	store := NewStore()
	p := NewPipeline(testRegistry(), store)
	asset := frameAsset(t, "corrupt", 5000, map[int]bool{1: true}, false)

	first := p.RunAsset(asset)
	// error model originates in the frame adapter …
	var fromAdapter bool
	for _, f := range first.Faults {
		if f.Code == model.FaultHashMismatch && f.Adapter == "frame-seq@v1" && f.Segment == 1 {
			fromAdapter = true
		}
	}
	if !fromAdapter {
		t.Fatalf("fault did not propagate from adapter: %v", first.Faults)
	}
	// … through repair into the remux manifest
	if first.Segments[1].Status != manifest.StatusCorrupt {
		t.Fatalf("segment 1 not marked corrupt: %+v", first.Segments[1])
	}
	if first.Segments[0].Status != manifest.StatusOK || !first.Segments[0].HashMatch() {
		t.Fatal("valid segment before corruption must verify")
	}
	if first.Segments[2].Status != manifest.StatusOK || !first.Segments[2].HashMatch() {
		t.Fatal("valid segment after corruption must verify")
	}

	// rerun on the same input: identical segments, index and hashes
	second := p.RunAsset(asset)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("rerun diverged")
	}

	// damage a checkpoint: recompute from the nearest valid boundary
	cp := store.m["corrupt"][0]
	cp.InputHash[0] ^= 0xFF
	store.m["corrupt"][0] = cp
	third := p.RunAsset(asset)
	if !reflect.DeepEqual(first, third) {
		t.Fatal("recompute after checkpoint damage diverged")
	}
}

func TestBoundaryAudioConflictPropagates(t *testing.T) {
	p := NewPipeline(testRegistry(), NewStore())
	res := p.RunAsset(frameAsset(t, "audio", 5000, nil, true))
	if !res.Faults.HasCode(model.FaultAudioIsolated) {
		t.Fatalf("audio conflict not surfaced: %v", res.Faults)
	}
	var isolated bool
	for _, d := range res.AudioDecisions {
		if d.Segment == 1 && d.Action == "isolate" && d.Original.Layout == "5.1" {
			isolated = true
		}
	}
	if !isolated {
		t.Fatal("isolation decision with original metadata missing")
	}
	// video path unaffected
	for _, e := range res.Segments {
		if e.Status != manifest.StatusOK {
			t.Fatalf("audio isolation must not corrupt video: %+v", e)
		}
	}
}

func TestBoundaryBoxTruncate(t *testing.T) {
	full, err := mp4adapter.Adapter{}.Marshal(
		[][]byte{[]byte("aaaa"), []byte("bbbb")}, []uint32{1}, model.RTPTimebase, 100)
	if err != nil {
		t.Fatal(err)
	}
	p := NewPipeline(testRegistry(), NewStore())
	res := p.RunAsset(Asset{
		Name: "frag.mp4", Kind: "mp4", MP4Data: full[:len(full)/2],
	})
	if !res.Faults.HasCode(model.FaultBoxTruncated) {
		t.Fatalf("truncation not located: %v", res.Faults)
	}
	if res.CorruptRanges != 1 || res.Segments[0].Status != manifest.StatusCorrupt {
		t.Fatalf("truncated asset must be corrupt, not complete: %+v", res.Segments)
	}
}

func TestUnknownAdapterRejectedBatchUnaffected(t *testing.T) {
	reg := adapter.NewRegistry() // nothing registered
	p := NewPipeline(reg, NewStore())
	bad := p.RunAsset(frameAsset(t, "no-adapter", 5000, nil, false))
	if !bad.Faults.HasCode(model.FaultUnknownAdapter) {
		t.Fatal("missing adapter must be rejected with unknown_adapter")
	}
	found := false
	for _, f := range bad.Faults {
		if f.Adapter == "frame-seq@v1" {
			found = true
		}
	}
	if !found {
		t.Fatal("fault must report the adapter identity")
	}

	p2 := NewPipeline(testRegistry(), NewStore())
	good := p2.RunAsset(frameAsset(t, "fine", 5000, nil, false))
	if good.CorruptRanges != 0 {
		t.Fatal("other assets in the batch must be unaffected")
	}
}
