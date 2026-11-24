package demux

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"testing"

	"mediadeliver/internal/fixture"
	"mediadeliver/internal/model"
)

func testLimits() model.Limits {
	l := model.DefaultLimits()
	l.MaxPacketBytes = 4096
	return l
}

func twoTrackSpecs() []fixture.TrackSpec {
	return []fixture.TrackSpec{
		{ID: 1, Video: true, Codec: "avc1", Timescale: 90000, Samples: 12,
			KeyInterval: 4, PayloadBytes: 40, WithElst: true, MediaTime: 500},
		{ID: 2, Video: false, Codec: "mp4a", Timescale: 48000, Samples: 12,
			PayloadBytes: 32},
	}
}

func TestMP4ProbeAndRead(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	data := fixture.BuildMP4(twoTrackSpecs(), rng)
	r := NewMP4Reader("cam1", bytes.NewReader(data), testLimits())

	infos, err := r.Probe()
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("want 2 tracks, got %d", len(infos))
	}
	byID := map[uint32]model.TrackInfo{}
	for _, inf := range infos {
		byID[inf.ID] = inf
	}
	v, ok := byID[1]
	if !ok || v.Kind != model.KindVideo || v.Codec != "avc1" || v.Timescale != 90000 {
		t.Fatalf("video track wrong: %+v", v)
	}
	a, ok := byID[2]
	if !ok || a.Kind != model.KindAudio || a.Codec != "mp4a" {
		t.Fatalf("audio track wrong: %+v", a)
	}

	pkts, err := r.ReadAll(context.Background())
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	if len(pkts) != 24 {
		t.Fatalf("want 24 packets, got %d", len(pkts))
	}
	// First video sample must carry the non-zero elst origin (500 ticks).
	var firstVideo *model.Packet
	keyCount := 0
	videoSeen := 0
	for _, p := range pkts {
		if p.TrackID == 1 {
			if firstVideo == nil {
				firstVideo = p
			}
			videoSeen++
			if p.Keyframe {
				keyCount++
			}
			if p.Timescale != 90000 {
				t.Fatalf("video timescale: %d", p.Timescale)
			}
		}
	}
	if firstVideo == nil || firstVideo.DTS != 500 {
		t.Fatalf("non-zero origin not preserved: %+v", firstVideo)
	}
	if keyCount != 3 { // samples 1,5,9 (1-based) -> 3 keyframes
		t.Fatalf("want 3 keyframes, got %d", keyCount)
	}
	if videoSeen != 12 {
		t.Fatalf("video packet count: %d", videoSeen)
	}
}

func TestMP4DeterministicPayloads(t *testing.T) {
	specs := twoTrackSpecs()
	d1 := fixture.BuildMP4(specs, rand.New(rand.NewSource(7)))
	d2 := fixture.BuildMP4(specs, rand.New(rand.NewSource(7)))
	if !bytes.Equal(d1, d2) {
		t.Fatal("identical seeds must produce byte-identical MP4 fixtures")
	}
}

func TestMP4CorruptLengthPropagates(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	data := fixture.BuildMP4(twoTrackSpecs(), rng)
	// Shrink the mdat box (top-level index 2) so its declared extent no longer
	// covers the sample payloads that its own stco/stsz point into.
	corrupt, err := fixture.CorruptMP4Length(data, 2, -128)
	if err != nil {
		t.Fatal(err)
	}
	r := NewMP4Reader("cam1", bytes.NewReader(corrupt), testLimits())
	if _, err := r.Probe(); err != nil {
		t.Fatalf("mdat length shrink should not break table parse: %v", err)
	}
	// Reading samples must now fail because declared offsets run past the
	// shrunk mdat extent; the error stays KindCorruptMedia with a position.
	_, err = r.ReadAll(context.Background())
	if model.KindOf(err) != model.KindCorruptMedia {
		t.Fatalf("want KindCorruptMedia when reading shrunk mdat, got %v", err)
	}
	if model.PositionOf(err) < 0 {
		t.Fatalf("corruption must preserve first error position: %v", err)
	}
}

func TestMP4TruncatedMdatPayload(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	data := fixture.BuildMP4(twoTrackSpecs(), rng)
	// Drop trailing bytes so a declared sample runs past EOF.
	truncated := data[:len(data)-10]
	r := NewMP4Reader("cam1", bytes.NewReader(truncated), testLimits())
	// The mdat box now overruns the file; the corruption surfaces during the
	// box walk and must carry the KindCorruptMedia classification.
	_, err := r.Probe()
	if model.KindOf(err) != model.KindCorruptMedia {
		t.Fatalf("want KindCorruptMedia on truncated mdat, got %v (kind=%v)", err, model.KindOf(err))
	}
	if model.PositionOf(err) < 0 {
		t.Fatalf("truncation must preserve first error position: %v", err)
	}
}

func TestRTPReadAndOrigin(t *testing.T) {
	specs := fixture.RTPSequence(8, 1000, 160, 0xabcdef, 4)
	capBytes := fixture.BuildRTPCapture(specs)
	rd := NewRTPReader("rec1", 11, model.KindAudio, "mp4a", 48000,
		bytes.NewReader(capBytes), testLimits())
	pkts, err := rd.ReadAll(context.Background())
	if err != nil {
		t.Fatalf("read rtp: %v", err)
	}
	if len(pkts) != 8 {
		t.Fatalf("want 8 packets, got %d", len(pkts))
	}
	if pkts[0].DTS != 1000 {
		t.Fatalf("non-zero RTP origin not preserved: %d", pkts[0].DTS)
	}
	if pkts[1].DTS != 1160 || pkts[1].Duration != 160 {
		t.Fatalf("timestamp/duration wrong: dts=%d dur=%d", pkts[1].DTS, pkts[1].Duration)
	}
	markerHits := 0
	for _, p := range pkts {
		if p.Keyframe {
			markerHits++
		}
		if p.SourceID != "rec1" || p.TrackID != 11 {
			t.Fatalf("provenance lost: %+v", p)
		}
	}
	if markerHits != 2 { // indices 0 and 4
		t.Fatalf("want 2 marker packets, got %d", markerHits)
	}
}

func TestRTPTruncatedPacketPropagates(t *testing.T) {
	specs := fixture.RTPSequence(6, 0, 160, 1, 2)
	capBytes := fixture.BuildRTPCapture(specs)
	// Torn write: capture ends inside packet 3; its length prefix stays valid
	// but the body no longer reaches the declared length.
	corrupt, err := fixture.TruncateRTPCapture(capBytes, 3)
	if err != nil {
		t.Fatal(err)
	}
	rd := NewRTPReader("rec1", 11, model.KindAudio, "mp4a", 48000,
		bytes.NewReader(corrupt), testLimits())
	_, err = rd.ReadAll(context.Background())
	if model.KindOf(err) != model.KindCorruptMedia {
		t.Fatalf("want KindCorruptMedia, got %v", err)
	}
	if model.PositionOf(err) != 3 {
		t.Fatalf("first error position must be packet 3, got %d", model.PositionOf(err))
	}
}

func TestRTPCaptureTornWrite(t *testing.T) {
	specs := fixture.RTPSequence(5, 0, 160, 1, 0)
	capBytes := fixture.BuildRTPCapture(specs)
	torn, err := fixture.TruncateRTPCapture(capBytes, 4)
	if err != nil {
		t.Fatal(err)
	}
	rd := NewRTPReader("rec1", 11, model.KindAudio, "mp4a", 48000,
		bytes.NewReader(torn), testLimits())
	_, err = rd.ReadAll(context.Background())
	if model.KindOf(err) != model.KindCorruptMedia || model.PositionOf(err) != 4 {
		t.Fatalf("want corruption at packet 4, got %v", err)
	}
}

func TestRTPOversizeRejected(t *testing.T) {
	specs := fixture.RTPSequence(2, 0, 160, 1, 0)
	capBytes := fixture.BuildRTPCapture(specs)
	limits := testLimits()
	limits.MaxPacketBytes = 8 // payloads are 24 bytes
	rd := NewRTPReader("rec1", 11, model.KindAudio, "mp4a", 48000,
		bytes.NewReader(capBytes), limits)
	_, err := rd.ReadAll(context.Background())
	if model.KindOf(err) != model.KindLimitExceeded {
		t.Fatalf("want KindLimitExceeded, got %v", err)
	}
}

func TestRTPRolloverExtension(t *testing.T) {
	specs := []fixture.RTPPacketSpec{
		{Timestamp: 0xfffffff0, Seq: 0, SSRC: 1, PayloadN: 10},
		{Timestamp: 0x00000010, Seq: 1, SSRC: 1, PayloadN: 10},
	}
	capBytes := fixture.BuildRTPCapture(specs)
	rd := NewRTPReader("rec1", 11, model.KindAudio, "mp4a", 48000,
		bytes.NewReader(capBytes), testLimits())
	pkts, err := rd.ReadAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pkts[1].DTS != int64(0x100000010) {
		t.Fatalf("rollover not extended: dts=%d want %d", pkts[1].DTS, int64(0x100000010))
	}
	if pkts[1].Duration != 0x20 {
		t.Fatalf("rollover duration: %d", pkts[1].Duration)
	}
}

func TestCancelStopsDemux(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	data := fixture.BuildMP4(twoTrackSpecs(), rng)
	r := NewMP4Reader("cam1", bytes.NewReader(data), testLimits())
	if _, err := r.Probe(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.ReadPacket(ctx); model.KindOf(err) != model.KindCancelled {
		t.Fatalf("want KindCancelled, got %v", err)
	}
	// RTP path honors cancellation too.
	rd := NewRTPReader("r", 1, model.KindAudio, "mp4a", 48000, bytes.NewReader(nil), testLimits())
	if _, err := rd.ReadPacket(ctx); err != io.EOF && model.KindOf(err) != model.KindCancelled {
		t.Fatalf("empty rtp: %v", err)
	}
}
