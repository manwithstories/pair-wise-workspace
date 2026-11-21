package ebml

import (
	"bytes"
	"io"
	"testing"

	"mkvarchive/internal/model"
)

// buildCluster emits one known- or unknown-length cluster with an explicit
// timecode and two interleaved SimpleBlocks (video track 1, audio track 2).
func buildCluster(t *testing.T, tc uint64, unknown bool) []byte {
	t.Helper()
	vFrame := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	aFrame := []byte{0xCA, 0xFE}
	body := append(uintElem(model.IDClusterTimecode, tc),
		elemBytes(model.IDSimpleBlock, simpleBlock(1, 0, 0x80, vFrame))...)
	body = append(body, elemBytes(model.IDSimpleBlock, simpleBlock(2, 5, 0x00, aFrame))...)
	if unknown {
		return elemUnknown(model.IDCluster, body, 8)
	}
	return elemBytes(model.IDCluster, body)
}

func TestScannerInterleavedBlocksAndOffsets(t *testing.T) {
	var src bytes.Buffer
	src.Write(minimalSegment(1_000_000))
	src.Write(buildCluster(t, 0, true))
	src.Write(buildCluster(t, 1000, false))

	sc, err := NewScanner(bytes.NewReader(src.Bytes()))
	if err != nil {
		t.Fatalf("scanner: %v", err)
	}
	if sc.Meta().TimestampScale != 1_000_000 {
		t.Fatalf("timestamp scale: %d", sc.Meta().TimestampScale)
	}
	var blocks []*Block
	for {
		blk, err := sc.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		blocks = append(blocks, blk)
	}
	if len(blocks) != 4 {
		t.Fatalf("want 4 interleaved blocks, got %d", len(blocks))
	}
	if blocks[0].TrackNumber != 0 || blocks[1].TrackNumber != 1 {
		t.Fatalf("track numbers not interleaved: %d %d", blocks[0].TrackNumber, blocks[1].TrackNumber)
	}
	if blocks[2].ClusterTimecode != 1000 {
		t.Fatalf("second cluster timecode not applied: %d", blocks[2].ClusterTimecode)
	}
	if !bytes.Equal(blocks[0].Payload[blocks[0].Size-4:], []byte{0xDE, 0xAD, 0xBE, 0xEF}) {
		t.Fatal("frame payload not retained verbatim")
	}
	if blocks[0].ElementHeaderOff <= 0 || blocks[1].PayloadOff <= blocks[0].ElementHeaderOff {
		t.Fatalf("source offsets not monotonic: %+v %+v", blocks[0], blocks[1])
	}
	if blocks[0].AbsoluteTimecode() != 0 || blocks[1].AbsoluteTimecode() != 5 {
		t.Fatalf("absolute timecodes wrong: %d %d", blocks[0].AbsoluteTimecode(), blocks[1].AbsoluteTimecode())
	}
	if !blocks[0].Keyframe || blocks[1].Keyframe {
		t.Fatal("keyframe flag parsing wrong")
	}
}

func TestScannerTruncatedTailKeepsConfirmedBlocks(t *testing.T) {
	var src bytes.Buffer
	src.Write(minimalSegment(1_000_000))
	src.Write(buildCluster(t, 0, false))
	goodLen := src.Len()
	// A broken 4-byte VINT marker with no following bytes.
	src.WriteByte(0x10)

	sc, err := NewScanner(bytes.NewReader(src.Bytes()))
	if err != nil {
		t.Fatalf("scanner: %v", err)
	}
	var n int
	var firstErr error
	for {
		blk, err := sc.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			firstErr = err
			break
		}
		if blk == nil {
			t.Fatal("nil block without error")
		}
		n++
	}
	if n != 2 {
		t.Fatalf("want the 2 confirmed blocks before damage, got %d", n)
	}
	ae, ok := firstErr.(*model.ArchiveError)
	if !ok || ae.Code != "EBML_TRUNCATED_HEADER" || ae.Offset != int64(goodLen) {
		t.Fatalf("want truncation at %d, got %v", goodLen, firstErr)
	}
	if sc.FirstError() == nil || sc.FirstError().Offset != int64(goodLen) {
		t.Fatalf("FirstError not retained: %+v", sc.FirstError())
	}
}

func TestScannerBlockOverrun(t *testing.T) {
	// Cluster is internally consistent, but its SimpleBlock declares 50
	// payload bytes while the whole stream ends after 3 of them.
	var src bytes.Buffer
	src.Write(minimalSegment(1_000_000))
	var cluster bytes.Buffer
	cluster.Write(uintElem(model.IDClusterTimecode, 0))
	sbID, _ := EncodeID(model.IDSimpleBlock)
	sbSize, _ := EncodeVINT(50)
	cluster.Write(sbID)
	cluster.Write(sbSize)
	cluster.Write([]byte{0x81, 0x00, 0x00})
	// Cluster size must cover the declared block, so use unknown length.
	src.Write(elemUnknown(model.IDCluster, cluster.Bytes(), 8))

	sc, err := NewScanner(bytes.NewReader(src.Bytes()))
	if err != nil {
		t.Fatalf("scanner: %v", err)
	}
	if _, err = sc.Next(); err == nil {
		t.Fatal("want overrun error for lying block size")
	} else if ae, ok := err.(*model.ArchiveError); !ok || ae.Code != "EBML_OVERRUN" {
		t.Fatalf("want EBML_OVERRUN, got %v", err)
	}
}

func TestScannerBlockGroup(t *testing.T) {
	var src bytes.Buffer
	src.Write(minimalSegment(1_000_000))
	blockPayload := simpleBlock(1, 10, 0x00, []byte{1, 2, 3, 4})
	group := elemBytes(model.IDBlock, blockPayload)
	cluster := append(uintElem(model.IDClusterTimecode, 500),
		elemBytes(model.IDBlockGroup, group)...)
	src.Write(elemBytes(model.IDCluster, cluster))

	sc, err := NewScanner(bytes.NewReader(src.Bytes()))
	if err != nil {
		t.Fatalf("scanner: %v", err)
	}
	blk, err := sc.Next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if blk.ElementID != model.IDBlock || blk.AbsoluteTimecode() != 510 {
		t.Fatalf("block group parse wrong: %+v", blk)
	}
	if _, err = sc.Next(); err != io.EOF {
		t.Fatalf("want clean EOF, got %v", err)
	}
}
