// Package integration exercises the whole pipeline against the module-level
// contracts: stream copy, hash verification, watermark retraction, checkpoint
// resume, and RTP arrival correlation.
package integration

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/pion/rtp"

	"mediapipe/internal/audit"
	"mediapipe/internal/checkpoint"
	"mediapipe/internal/demux"
	"mediapipe/internal/mapping"
	"mediapipe/internal/media"
	"mediapipe/internal/remux"
	rtpmod "mediapipe/internal/rtp"
	"mediapipe/internal/stream"
	"mediapipe/internal/testkit"
	"mediapipe/internal/timeline"
)

// loadStream demuxes a fixture and resolves the default mapping.
// packetKey is the stable dedup key used by the watermark engine in this test
// suite.
func packetKey(p media.Packet) string {
	return fmt.Sprintf("%d/%d/%d", p.Track, p.DTS, p.Sequence)
}

func loadStream(t *testing.T, data []byte, target mapping.Target) (*demux.Result, *mapping.Plan, []media.Packet) {
	t.Helper()
	in, err := demux.Demux(bytes.NewReader(data), demux.DefaultLimits())
	if err != nil {
		t.Fatalf("demux: %v", err)
	}
	tracks := in.Tracks
	rules := mapping.DefaultRules()
	if target == mapping.TargetFMP4 {
		var kept []media.Track
		for _, tr := range tracks {
			if tr.Type == media.TrackSubtitle {
				continue
			}
			kept = append(kept, tr)
		}
		tracks = kept
	}
	plan, err := mapping.NewResolver(target, rules).Resolve(tracks)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var packets []media.Packet
	for _, p := range in.Packets {
		if _, ok := plan.Remap(p.Track); ok {
			packets = append(packets, p)
		}
	}
	return in, plan, packets
}

func TestStreamCopyIsByteIdentical(t *testing.T) {
	clean, _, _, _ := testkit.Generate(73)
	in, plan, packets := loadStream(t, clean.Bytes, mapping.TargetMatroska)
	var out bytes.Buffer
	sink, err := remux.NewSink(&out, plan, in.Tracks, remux.Options{
		Target: mapping.TargetMatroska, ClusterSpan: 2 * media.Second, ClusterCRC: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range packets {
		if err := sink.WritePacket(p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := demux.Demux(bytes.NewReader(out.Bytes()), demux.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Packets) != len(packets) {
		t.Fatalf("packet count %d != %d", len(again.Packets), len(packets))
	}
	// Every output payload must equal a source payload byte-for-byte. Content
	// matching is enough to prove no decode/re-encode happened, and multiset
	// semantics tolerate the writer's cluster reordering.
	src := make(map[string]int, len(packets))
	for _, p := range packets {
		src[string(p.Data)]++
	}
	for _, p := range again.Packets {
		key := string(p.Data)
		if src[key] == 0 {
			t.Fatalf("output packet track=%d dts=%d is not a verbatim source payload", p.Track, p.DTS)
		}
		src[key]--
	}
	for _, n := range src {
		if n != 0 {
			t.Fatalf("%d source payloads missing from output", n)
		}
	}
}

func TestWatermarkRetractionThenRemux(t *testing.T) {
	_, late, _, _ := testkit.Generate(73)
	in, plan, packets := loadStream(t, late.Bytes, mapping.TargetMatroska)
	engine, err := timeline.New(timeline.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range packets {
		if _, _, err := engine.Push(timeline.Event{
			Key: packetKey(p), Track: p.Track, Time: p.DTS,
			Duration: p.Duration, Payload: p.Data, Arrival: p.Sequence,
		}); err != nil {
			t.Fatalf("watermark rejected a within-horizon frame: %v", err)
		}
	}
	if engine.Stats().LateAccepted == 0 {
		t.Fatal("late fixture produced no late arrivals")
	}
	// The engine's final timeline must cover the same span the writer reports.
	var out bytes.Buffer
	sink, _ := remux.NewSink(&out, plan, in.Tracks, remux.Options{
		Target: mapping.TargetMatroska, ClusterSpan: 2 * media.Second,
	})
	for _, p := range packets {
		if err := sink.WritePacket(p); err != nil {
			t.Fatal(err)
		}
	}
	sum, err := sink.Close()
	if err != nil {
		t.Fatal(err)
	}
	start, end := engine.Timeline()
	if start > sum.TimeStart || end < sum.TimeEnd {
		t.Fatalf("engine timeline [%d,%d] does not cover writer [%d,%d]", start, end, sum.TimeStart, sum.TimeEnd)
	}
}

func TestBoundedQueueUnderBackpressure(t *testing.T) {
	clean, _, _, _ := testkit.Generate(73)
	in, plan, packets := loadStream(t, clean.Bytes, mapping.TargetMatroska)
	var out bytes.Buffer
	sink, _ := remux.NewSink(&out, plan, in.Tracks, remux.Options{Target: mapping.TargetMatroska})
	const budget = 64 << 10
	q := stream.NewQueue[media.Packet](budget, stream.PacketSizer(64))
	done := make(chan error, 1)
	go func() {
		for _, p := range packets {
			if err := q.Push(t.Context(), p); err != nil {
				done <- err
				return
			}
		}
		q.Close()
		done <- nil
	}()
	count := 0
	for {
		p, err := q.Pop(t.Context())
		if err != nil {
			break
		}
		if err := sink.WritePacket(p); err != nil {
			t.Fatal(err)
		}
		count++
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if count != len(packets) {
		t.Fatalf("consumed %d packets, want %d", count, len(packets))
	}
	if peak := q.Peak(); peak > budget {
		t.Fatalf("peak %d exceeded budget %d", peak, budget)
	}
}

func TestManifestCommitOnlyAfterVerification(t *testing.T) {
	clean, _, _, _ := testkit.Generate(73)
	in, plan, packets := loadStream(t, clean.Bytes, mapping.TargetMatroska)
	var out bytes.Buffer
	sink, _ := remux.NewSink(&out, plan, in.Tracks, remux.Options{
		Target: mapping.TargetMatroska, ClusterSpan: 2 * media.Second, ClusterCRC: true,
	})
	for _, p := range packets {
		sink.WritePacket(p)
	}
	sum, err := sink.Close()
	if err != nil {
		t.Fatal(err)
	}
	verify, err := remux.VerifySinkOutput(out.Bytes(), packets, plan)
	if err != nil {
		t.Fatal(err)
	}
	checked, bad, err := remux.VerifyCRCs(out.Bytes(), true)
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 || bad != 0 {
		t.Fatalf("crc checked=%d bad=%d", checked, bad)
	}
	b := audit.NewBuilder("integration", "fixture", mapping.TargetMatroska)
	b.SetOutput(sum)
	b.SetVerification(verify.Mismatches, bad)
	b.SetTracks(plan, packets)
	if err := b.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !b.Manifest().Committed {
		t.Fatal("manifest not committed")
	}
	// Corrupt one byte in the payload and confirm the gate fails.
	corrupt := append([]byte(nil), out.Bytes()...)
	corrupt[len(corrupt)/2] ^= 0xFF
	if _, err := remux.VerifySinkOutput(corrupt, packets, plan); err == nil {
		// Verification may surface the change as a mismatch rather than an
		// error; either way, the gate must not report zero.
		if res, _ := remux.VerifySinkOutput(corrupt, packets, plan); res.Mismatches == 0 && res.CRCErrors == 0 {
			t.Fatal("corrupted output passed verification")
		}
	}
}

func TestResumeAcrossCheckpointStore(t *testing.T) {
	clean, _, _, _ := testkit.Generate(73)
	in, plan, packets := loadStream(t, clean.Bytes, mapping.TargetMatroska)
	store, err := checkpoint.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts := remux.Options{Target: mapping.TargetMatroska, ClusterSpan: 2 * media.Second}

	var ref bytes.Buffer
	refSink, _ := remux.NewSink(&ref, plan, in.Tracks, opts)
	for _, p := range packets {
		refSink.WritePacket(p)
	}
	refSum, _ := refSink.Close()

	var part bytes.Buffer
	s1, _ := remux.NewSink(&part, plan, in.Tracks, opts)
	for _, p := range packets[:len(packets)/2] {
		s1.WritePacket(p)
	}
	prog, err := s1.Flush()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("job", "out", "matroska", prog); err != nil {
		t.Fatal(err)
	}
	st, err := store.Load("job")
	if err != nil || st == nil {
		t.Fatalf("load: %v", err)
	}
	resumeOpts := opts
	resumeOpts.Resume = st.Resume()
	var out bytes.Buffer
	out.Write(part.Bytes()[:prog.Bytes])
	s2, err := remux.NewSink(&out, plan, in.Tracks, resumeOpts)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range packets[prog.Packets:] {
		if err := s2.WritePacket(p); err != nil {
			t.Fatal(err)
		}
	}
	resSum, err := s2.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resSum.Digest != refSum.Digest {
		t.Fatalf("resume digest %x != full %x", resSum.Digest[:8], refSum.Digest[:8])
	}
}

func TestRTPArrivalsCorrelateWithPackets(t *testing.T) {
	// Encode the first N video packets as RTP arrivals and confirm the parser
	// recovers arrival times and detects a deliberate loss.
	clean, _, _, _ := testkit.Generate(73)
	_, plan, packets := loadStream(t, clean.Bytes, mapping.TargetMatroska)
	video := make([]media.Packet, 0, 64)
	for _, p := range packets {
		if route, ok := plan.Lookup(p.Track); ok && route.OutType == media.TrackVideo {
			video = append(video, p)
			if len(video) == 16 {
				break
			}
		}
	}
	if len(video) < 4 {
		t.Fatalf("fixture has only %d video packets", len(video))
	}
	var records [][]byte
	for i, p := range video {
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version: 2, PayloadType: 96, SequenceNumber: uint16(i),
				Timestamp: uint32(media.ToScale(p.DTS, 90000)), SSRC: 0x1234,
			},
			Payload: p.Data,
		}
		// Skip record 3 to simulate a lost packet.
		if i == 3 {
			continue
		}
		rec, err := rtpmod.BuildRecord(p.DTS+5*media.Millisecond, 90000, pkt)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, rec)
	}
	var capture bytes.Buffer
	if err := rtpmod.WriteCapture(&capture, records); err != nil {
		t.Fatal(err)
	}
	parsed, err := rtpmod.Parse(&capture, rtpmod.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Arrivals) != len(video)-1 {
		t.Fatalf("arrivals=%d want %d", len(parsed.Arrivals), len(video)-1)
	}
	if parsed.Lost != 1 || parsed.Discontinuity != 1 {
		t.Fatalf("gap detection: lost=%d discontinuity=%d", parsed.Lost, parsed.Discontinuity)
	}
	for _, a := range parsed.Arrivals {
		if a.SSRC != 0x1234 || a.ClockRate != 90000 {
			t.Fatalf("arrival fields wrong: %+v", a)
		}
		if a.PayloadSize == 0 {
			t.Fatal("arrival has empty payload")
		}
	}
}

func TestTruncatedInputNeverProducesOutput(t *testing.T) {
	_, _, truncated, _ := testkit.Generate(73)
	in, err := demux.Demux(bytes.NewReader(truncated.Bytes), demux.DefaultLimits())
	if err == nil {
		t.Fatal("truncated fixture demuxed cleanly")
	}
	if !in.Truncated {
		t.Fatal("Truncated flag not set")
	}
	if media.ExitCode(err) != 2 {
		t.Fatalf("exit=%d", media.ExitCode(err))
	}
}
