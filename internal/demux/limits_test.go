package demux

import (
	"bytes"
	"testing"

	"mediapipe/internal/ebml"
	"mediapipe/internal/media"
	"mediapipe/internal/testkit"
)

func TestDepthLimitRejectsNesting(t *testing.T) {
	// Segment > Tracks > TrackEntry > Video is four levels deep; a limit of
	// three must reject the file at the Video child's offset.
	clean, _, _, _ := testkit.Generate(73)
	lim := DefaultLimits()
	lim.EBML.MaxDepth = 3
	_, err := Demux(bytes.NewReader(clean.Bytes), lim)
	if err == nil {
		t.Fatal("expected depth error")
	}
	if media.ClassOf(err) != media.ClassFatal {
		t.Fatalf("class=%v", media.ClassOf(err))
	}
	if off, ok := media.OffsetOf(err); !ok || off <= 0 {
		t.Fatalf("depth error offset=%d ok=%v", off, ok)
	}
}

func TestDepthLimitAllowsShallowFile(t *testing.T) {
	clean, _, _, _ := testkit.Generate(73)
	lim := DefaultLimits()
	lim.EBML.MaxDepth = 6
	_, err := Demux(bytes.NewReader(clean.Bytes), lim)
	if err != nil {
		t.Fatalf("legitimate file rejected at depth 6: %v", err)
	}
}

func TestIllegalElementLengthRejected(t *testing.T) {
	// A SimpleBlock whose declared size is larger than the remaining bytes.
	var buf bytes.Buffer
	w := ebml.NewWriter(&buf, nil)
	w.StartContainerUnknown(ebml.IDSegment)
	w.Element(ebml.IDTracks, nil)
	w.StartContainerUnknown(ebml.IDCluster)
	w.Uint(ebml.IDTimestamp, 0)
	w.StartElement(ebml.IDSimpleBlock, 100)   // claims 100 bytes
	buf.Write([]byte{0x81, 0x00, 0x00, 0x80}) // supplies 4
	_, err := Demux(bytes.NewReader(buf.Bytes()), DefaultLimits())
	if err == nil {
		t.Fatal("expected truncation error")
	}
	if media.ClassOf(err) != media.ClassFatal {
		t.Fatalf("class=%v", media.ClassOf(err))
	}
}

func TestBlockForUndeclaredTrackRejected(t *testing.T) {
	var buf bytes.Buffer
	w := ebml.NewWriter(&buf, nil)
	w.StartContainerUnknown(ebml.IDSegment)
	// Tracks section with only track 1.
	var tracks bytes.Buffer
	tw := ebml.NewWriter(&tracks, nil)
	var entry bytes.Buffer
	ew := ebml.NewWriter(&entry, nil)
	ew.Uint(ebml.IDTrackNumber, 1)
	ew.Uint(ebml.IDTrackType, uint64(media.TrackVideo))
	ew.String(ebml.IDCodecID, string(media.CodecAVC))
	ew.Uint(ebml.IDDefaultDuration, 40_000_000)
	tw.Element(ebml.IDTrackEntry, entry.Bytes())
	w.Element(ebml.IDTracks, tracks.Bytes())
	w.StartContainerUnknown(ebml.IDCluster)
	w.Uint(ebml.IDTimestamp, 0)
	// SimpleBlock referring to track 9.
	var block bytes.Buffer
	enc, _ := ebml.EncodeSize(9)
	block.Write(enc)
	block.Write([]byte{0, 0, 0x80, 0xDE, 0xAD})
	w.Element(ebml.IDSimpleBlock, block.Bytes())
	_, err := Demux(bytes.NewReader(buf.Bytes()), DefaultLimits())
	if err == nil || media.ClassOf(err) != media.ClassFatal {
		t.Fatalf("err=%v", err)
	}
}

func TestTrackCountLimit(t *testing.T) {
	clean, _, _, _ := testkit.Generate(73)
	lim := DefaultLimits()
	lim.MaxTrackCount = 2
	_, err := Demux(bytes.NewReader(clean.Bytes), lim)
	if err == nil || media.ClassOf(err) != media.ClassFatal {
		t.Fatalf("err=%v", err)
	}
}

func TestPacketCountLimit(t *testing.T) {
	clean, _, _, _ := testkit.Generate(73)
	lim := DefaultLimits()
	lim.MaxPackets = 10
	_, err := Demux(bytes.NewReader(clean.Bytes), lim)
	if err == nil || media.ClassOf(err) != media.ClassFatal {
		t.Fatalf("err=%v", err)
	}
}

func TestUnknownSizeTopLevelContainerRejected(t *testing.T) {
	// An unknown-size element that is neither Segment nor Cluster has no
	// defined boundary and must be rejected rather than silently skipped.
	var buf bytes.Buffer
	w := ebml.NewWriter(&buf, nil)
	w.StartContainerUnknown(ebml.IDTags)
	_, err := Demux(bytes.NewReader(buf.Bytes()), DefaultLimits())
	if err == nil || media.ClassOf(err) != media.ClassFatal {
		t.Fatalf("err=%v", err)
	}
}
