package remux

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"

	"mkvarchive/internal/ebml"
	"mkvarchive/internal/model"
	"mkvarchive/internal/timeline"
	"mkvarchive/internal/track"
)

func digestForTest(frame []byte) string {
	sum := sha256.Sum256(frame)
	return hex.EncodeToString(sum[:])
}

// buildSourceMKV creates a two-track (video uid 1, audio uid 2) stream with
// unknown-length segment/clusters and interleaved blocks.
func buildSourceMKV(t *testing.T) ([]byte, []byte, []byte) {
	t.Helper()
	vEntry := trackEntryForTest(1, 1, model.TrackTypeVideo, "V_VP9")
	aEntry := trackEntryForTest(2, 2, model.TrackTypeAudio, "A_OPUS")
	tracksData := append(vEntry, aEntry...)
	infoData := ebml.EncodeElement(model.IDTimestampScale, ebml.EncodeUnsigned(1_000_000))

	var body bytes.Buffer
	body.Write(infoData)
	body.Write(ebml.EncodeElement(model.IDTracks, tracksData))

	addCluster := func(tc uint64, blocks ...[]byte) {
		var c bytes.Buffer
		c.Write(ebml.EncodeElement(model.IDClusterTimecode, ebml.EncodeUnsigned(tc)))
		for _, blk := range blocks {
			c.Write(ebml.EncodeElement(model.IDSimpleBlock, blk))
		}
		body.Write(ebml.EncodeElementUnknown(model.IDCluster, c.Bytes(), 8))
	}
	addCluster(0,
		simpleBlockForTest(1, 0, 0x80, []byte{0x11, 0x22, 0x33, 0x44}),
		simpleBlockForTest(2, 5, 0x00, []byte{0xAA, 0xBB}),
	)
	addCluster(1000,
		simpleBlockForTest(1, 0, 0x80, []byte{0x55, 0x66, 0x77, 0x88}),
		simpleBlockForTest(2, 5, 0x00, []byte{0xCC, 0xDD}),
	)

	var src bytes.Buffer
	src.Write(ebml.EncodeElement(model.IDEBMLHeader, []byte{0x42, 0x82, 0x88, 'm', 'a', 't', 'r', 'o', 's', 'k', 'a'}))
	src.Write(ebml.EncodeElementUnknown(model.IDSegment, body.Bytes(), 8))
	return src.Bytes(), infoData, tracksData
}

func trackEntryForTest(uid, number uint64, tt model.TrackType, codec string) []byte {
	entry := ebml.EncodeElement(model.IDTrackUID, ebml.EncodeUnsigned(uid))
	entry = append(entry, ebml.EncodeElement(model.IDTrackNumber, ebml.EncodeUnsigned(number))...)
	entry = append(entry, ebml.EncodeElement(model.IDTrackType, ebml.EncodeUnsigned(uint64(tt)))...)
	entry = append(entry, ebml.EncodeElement(model.IDCodecID, []byte(codec))...)
	return ebml.EncodeElement(model.IDTrackEntry, entry)
}

func simpleBlockForTest(trackNum uint64, relTC int16, flags byte, frame []byte) []byte {
	tn, _ := ebml.EncodeVINT(trackNum - 1)
	out := append([]byte{}, tn...)
	out = append(out, byte(uint16(relTC)>>8), byte(uint16(relTC)), flags)
	return append(out, frame...)
}

func TestRemuxRoundTripPayloadsVerbatim(t *testing.T) {
	src, infoData, tracksData := buildSourceMKV(t)
	tracks, err := track.ParseTracks(tracksData)
	if err != nil {
		t.Fatalf("parse tracks: %v", err)
	}
	g, err := track.BuildGraph(tracks)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	mappings, _, err := track.BuildMapping(g, track.Policy{})
	if err != nil {
		t.Fatalf("mapping: %v", err)
	}
	plan := NewPlan(1_000_000, mappings, nil)

	scanner, err := ebml.NewScanner(bytes.NewReader(src))
	if err != nil {
		t.Fatalf("scanner: %v", err)
	}
	builder := timeline.NewBuilder(plan.TimestampScale)
	var out bytes.Buffer
	writer, err := NewWriter(&out, plan, infoData, tracksData, builder)
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	expected := map[string]bool{}
	var n int
	for {
		blk, err := scanner.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		frame, _ := frameOf(blk.Payload)
		sum := digestForTest(frame)
		expected[sum] = true
		if err = writer.WriteBlock(BlockEnvelope{
			SourceTrackNumber: blk.TrackNumber, // on-wire VINT data value (number-1)
			ElementID:         blk.ElementID,
			ClusterTimecode:   blk.ClusterTimecode,
			RawTimecode:       blk.AbsoluteTimecode(),
			HeaderOffset:      blk.ElementHeaderOff,
			Payload:           blk.Payload,
		}); err != nil {
			t.Fatalf("write block: %v", err)
		}
		n++
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if n != 4 {
		t.Fatalf("want 4 blocks, got %d", n)
	}

	// Re-parse the archive copy and confirm frame payloads match source.
	reScan, err := ebml.NewScanner(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatalf("re-scan archive: %v", err)
	}
	var reBlocks int
	for {
		blk, err := reScan.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("re-scan block: %v", err)
		}
		frame, _ := frameOf(blk.Payload)
		if !expected[digestForTest(frame)] {
			t.Fatalf("archived frame payload changed: %x", frame)
		}
		reBlocks++
	}
	if reBlocks != 4 {
		t.Fatalf("archive block count: %d", reBlocks)
	}
	report := writer.QualityReport(12000, 2_000_000_000, expected, 0.0)
	if !report.PayloadSHA256Match || report.QualityBlocks != 4 {
		t.Fatalf("quality report wrong: %+v", report)
	}
	manifest := builder.Build()
	if len(manifest.Tracks) != 2 || manifest.Tracks[1].Blocks != 2 {
		t.Fatalf("manifest ranges wrong: %+v", manifest.Tracks)
	}
}

func TestWriterRejectsUnknownTrack(t *testing.T) {
	_, infoData, tracksData := buildSourceMKV(t)
	tracks, _ := track.ParseTracks(tracksData)
	g, _ := track.BuildGraph(tracks)
	mappings, _, _ := track.BuildMapping(g, track.Policy{})
	plan := NewPlan(1_000_000, mappings, nil)
	builder := timeline.NewBuilder(plan.TimestampScale)
	var out bytes.Buffer
	w, err := NewWriter(&out, plan, infoData, tracksData, builder)
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	err = w.WriteBlock(BlockEnvelope{
		SourceTrackNumber: 98, ElementID: model.IDSimpleBlock,
		ClusterTimecode: 0, Payload: simpleBlockForTest(99, 0, 0, []byte{1}),
	})
	if ae, ok := err.(*model.ArchiveError); !ok || ae.Code != "TRACK_MAPPING_FAILED" {
		t.Fatalf("want mapping failure, got %v", err)
	}
}
