package remux

import (
	"bytes"
	"testing"

	"mediapipe/internal/demux"
	"mediapipe/internal/mapping"
	"mediapipe/internal/media"
	"mediapipe/internal/testkit"
)

func demuxFixture(t *testing.T, data []byte) *demux.Result {
	t.Helper()
	res, err := demux.Demux(bytes.NewReader(data), demux.DefaultLimits())
	if err != nil {
		t.Fatalf("demux fixture: %v", err)
	}
	return res
}

func remapPackets(plan *mapping.Plan, in []media.Packet) []media.Packet {
	out := make([]media.Packet, 0, len(in))
	for _, p := range in {
		if _, ok := plan.Remap(p.Track); ok {
			out = append(out, p)
		}
	}
	return out
}

func writeAll(t *testing.T, plan *mapping.Plan, tracks []media.Track, packets []media.Packet, opts Options) ([]byte, Summary) {
	t.Helper()
	var buf bytes.Buffer
	sink, err := NewSink(&buf, plan, tracks, opts)
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	for _, p := range packets {
		if err := sink.WritePacket(p); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
	}
	sum, err := sink.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes(), sum
}

func TestMKVRoundTripPreservesPayloads(t *testing.T) {
	clean, _, _, _ := testkit.Generate(73)
	in := demuxFixture(t, clean.Bytes)
	plan, err := mapping.NewResolver(mapping.TargetMatroska, mapping.DefaultRules()).Resolve(in.Tracks)
	if err != nil {
		t.Fatal(err)
	}
	packets := remapPackets(plan, in.Packets)
	data, sum := writeAll(t, plan, in.Tracks, packets, Options{
		Target: mapping.TargetMatroska, ClusterSpan: 2 * media.Second, ClusterCRC: true,
	})
	if sum.Packets != len(packets) {
		t.Fatalf("packets=%d want %d", sum.Packets, len(packets))
	}
	res, err := VerifySinkOutput(data, packets, plan)
	if err != nil {
		t.Fatalf("VerifySinkOutput: %v", err)
	}
	if res.Mismatches != 0 || res.CRCErrors != 0 {
		t.Fatalf("verify=%+v", res)
	}
	if res.Matched != len(packets) {
		t.Fatalf("matched=%d want %d", res.Matched, len(packets))
	}
	checked, bad, err := VerifyCRCs(data, true)
	if err != nil || bad != 0 {
		t.Fatalf("crc checked=%d bad=%d err=%v", checked, bad, err)
	}
	if checked != sum.Clusters {
		t.Fatalf("crc checked=%d clusters=%d", checked, sum.Clusters)
	}
}

func TestMKVOutputIsDeterministic(t *testing.T) {
	clean, _, _, _ := testkit.Generate(73)
	in := demuxFixture(t, clean.Bytes)
	plan, _ := mapping.NewResolver(mapping.TargetMatroska, mapping.DefaultRules()).Resolve(in.Tracks)
	packets := remapPackets(plan, in.Packets)
	opts := Options{Target: mapping.TargetMatroska, ClusterSpan: 2 * media.Second, ClusterCRC: true}
	first, s1 := writeAll(t, plan, in.Tracks, packets, opts)
	second, s2 := writeAll(t, plan, in.Tracks, packets, opts)
	if s1.Digest != s2.Digest || !bytes.Equal(first, second) {
		t.Fatal("repeated remux produced different bytes")
	}
}

func TestMKVUnknownSizesRoundTrip(t *testing.T) {
	clean, _, _, _ := testkit.Generate(73)
	in := demuxFixture(t, clean.Bytes)
	plan, _ := mapping.NewResolver(mapping.TargetMatroska, mapping.DefaultRules()).Resolve(in.Tracks)
	packets := remapPackets(plan, in.Packets)
	data, _ := writeAll(t, plan, in.Tracks, packets, Options{
		Target: mapping.TargetMatroska, ClusterSpan: media.Second,
		UnknownSegment: true, UnknownCluster: true, ClusterCRC: true,
	})
	out := demuxFixture(t, data)
	if out.UnknownSizes < 2 {
		t.Fatalf("unknown sizes=%d", out.UnknownSizes)
	}
	if len(out.Packets) != len(packets) {
		t.Fatalf("packets=%d want %d", len(out.Packets), len(packets))
	}
	checked, bad, err := VerifyCRCs(data, true)
	if err != nil || bad != 0 || checked == 0 {
		t.Fatalf("crc checked=%d bad=%d err=%v", checked, bad, err)
	}
}

func TestMKVRewritesTrackNumbers(t *testing.T) {
	clean, _, _, _ := testkit.Generate(73)
	in := demuxFixture(t, clean.Bytes)
	rules := []mapping.Rule{
		{TrackNumber: 1, OutTrack: 11},
		{TrackNumber: 2, OutTrack: 12},
		{TrackNumber: 3, OutTrack: 13},
	}
	plan, err := mapping.NewResolver(mapping.TargetMatroska, rules).Resolve(in.Tracks)
	if err != nil {
		t.Fatal(err)
	}
	packets := remapPackets(plan, in.Packets)
	data, _ := writeAll(t, plan, in.Tracks, packets, Options{Target: mapping.TargetMatroska, ClusterSpan: media.Second})
	out := demuxFixture(t, data)
	ids := map[uint64]bool{}
	for _, tr := range out.Tracks {
		ids[tr.ID] = true
	}
	for _, want := range []uint64{11, 12, 13} {
		if !ids[want] {
			t.Fatalf("output missing track %d: %v", want, ids)
		}
	}
	res, err := VerifySinkOutput(data, packets, plan)
	if err != nil || res.Mismatches != 0 {
		t.Fatalf("verify=%+v err=%v", res, err)
	}
}

func TestMKVRejectsUnmappedPacket(t *testing.T) {
	clean, _, _, _ := testkit.Generate(73)
	in := demuxFixture(t, clean.Bytes)
	rules := []mapping.Rule{{TrackNumber: 1, OutTrack: 1}}
	plan, err := mapping.NewResolver(mapping.TargetMatroska, rules).Resolve(in.Tracks[:1])
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	sink, err := NewSink(&buf, plan, in.Tracks[:1], Options{Target: mapping.TargetMatroska})
	if err != nil {
		t.Fatal(err)
	}
	p := in.Packets[0]
	p.Track = 2
	err = sink.WritePacket(p)
	if err == nil || media.ClassOf(err) != media.ClassUnsupported {
		t.Fatalf("err=%v", err)
	}
}

func TestWebMOutput(t *testing.T) {
	clean, _, _, _ := testkit.Generate(73)
	in := demuxFixture(t, clean.Bytes)
	tracks := append([]media.Track(nil), in.Tracks[:2]...)
	tracks[0].Codec = media.CodecVP9
	plan, err := mapping.NewResolver(mapping.TargetWebM, mapping.DefaultRules()).Resolve(tracks)
	if err != nil {
		t.Fatal(err)
	}
	packets := remapPackets(plan, in.Packets)
	data, sum := writeAll(t, plan, tracks, packets, Options{Target: mapping.TargetWebM, ClusterSpan: media.Second})
	if sum.Target != mapping.TargetWebM {
		t.Fatalf("target=%s", sum.Target)
	}
	out := demuxFixture(t, data)
	if out.DocType != "webm" {
		t.Fatalf("docType=%q", out.DocType)
	}
	if len(out.Packets) != len(packets) {
		t.Fatalf("packets=%d want %d", len(out.Packets), len(packets))
	}
}

func TestFMP4RoundTripPreservesPayloads(t *testing.T) {
	clean, _, _, _ := testkit.Generate(73)
	in := demuxFixture(t, clean.Bytes)
	tracks := append([]media.Track(nil), in.Tracks[:2]...)
	plan, err := mapping.NewResolver(mapping.TargetFMP4, mapping.DefaultRules()).Resolve(tracks)
	if err != nil {
		t.Fatal(err)
	}
	packets := remapPackets(plan, in.Packets)
	data, sum := writeAll(t, plan, tracks, packets, Options{Target: mapping.TargetFMP4, ClusterSpan: 2 * media.Second})
	if sum.Packets != len(packets) {
		t.Fatalf("packets=%d want %d", sum.Packets, len(packets))
	}
	samples, err := Fragments(data)
	if err != nil {
		t.Fatalf("Fragments: %v", err)
	}
	if len(samples) != len(packets) {
		t.Fatalf("samples=%d want %d", len(samples), len(packets))
	}
	// Compare payloads by (track, DTS) like the Matroska verifier.
	type key struct {
		track uint32
		dts   media.Timestamp
	}
	want := make(map[key]uint32, len(packets))
	for _, p := range packets {
		out, _ := plan.Remap(p.Track)
		want[key{uint32(out), p.DTS}] = crcOf(p.Data)
	}
	for _, s := range samples {
		got, ok := want[key{s.TrackID, s.DTS}]
		if !ok {
			t.Fatalf("unexpected sample track=%d dts=%d", s.TrackID, s.DTS)
		}
		if got != crcOf(s.Data) {
			t.Fatalf("payload mismatch track=%d dts=%d", s.TrackID, s.DTS)
		}
		delete(want, key{s.TrackID, s.DTS})
	}
	if len(want) != 0 {
		t.Fatalf("%d source packets missing from fMP4 output", len(want))
	}
}

func TestFMP4Deterministic(t *testing.T) {
	clean, _, _, _ := testkit.Generate(73)
	in := demuxFixture(t, clean.Bytes)
	tracks := append([]media.Track(nil), in.Tracks[:2]...)
	plan, _ := mapping.NewResolver(mapping.TargetFMP4, mapping.DefaultRules()).Resolve(tracks)
	packets := remapPackets(plan, in.Packets)
	opts := Options{Target: mapping.TargetFMP4, ClusterSpan: media.Second}
	first, s1 := writeAll(t, plan, tracks, packets, opts)
	second, s2 := writeAll(t, plan, tracks, packets, opts)
	if s1.Digest != s2.Digest || !bytes.Equal(first, second) {
		t.Fatal("repeated fMP4 remux produced different bytes")
	}
}

func TestMP4RejectsSubtitleTarget(t *testing.T) {
	tracks := []media.Track{
		{ID: 1, Type: media.TrackSubtitle, Codec: media.CodecText, Language: "eng"},
	}
	_, err := mapping.NewResolver(mapping.TargetFMP4, mapping.DefaultRules()).Resolve(tracks)
	if err == nil || media.ClassOf(err) != media.ClassUnsupported {
		t.Fatalf("err=%v", err)
	}
}

func TestSinkCloseTwiceFails(t *testing.T) {
	clean, _, _, _ := testkit.Generate(73)
	in := demuxFixture(t, clean.Bytes)
	plan, _ := mapping.NewResolver(mapping.TargetMatroska, mapping.DefaultRules()).Resolve(in.Tracks)
	var buf bytes.Buffer
	sink, _ := NewSink(&buf, plan, in.Tracks, Options{Target: mapping.TargetMatroska})
	if _, err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Close(); err == nil {
		t.Fatal("second Close should fail")
	}
}

// summaryDigest is used to compare duplicate payloads without importing
// crc32 at every call site.
func crcOf(b []byte) uint32 {
	var h uint32 = 2166136261
	for _, c := range b {
		h ^= uint32(c)
		h *= 16777619
	}
	return h
}
