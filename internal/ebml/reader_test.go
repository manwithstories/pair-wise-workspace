package ebml

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// --- tests -----------------------------------------------------------------

func TestSizedMasterAndLeafRoundTrip(t *testing.T) {
	doc := elem(IDSegment,
		append(elem(IDInfo, elem(IDTimestampScale, []byte{0x40, 0x00})),
			elem(IDCluster, elem(IDTimecode, []byte{0x80}))...))
	rd := NewReader(bytes.NewReader(doc))

	seg, err := rd.Next()
	if err != nil || seg.ID != IDSegment {
		t.Fatalf("segment header: %v", err)
	}
	if err := rd.Enter(seg); err != nil {
		t.Fatal(err)
	}
	info, err := rd.Next()
	if err != nil || info.ID != IDInfo {
		t.Fatalf("info header: %v", err)
	}
	if err := rd.Enter(info); err != nil {
		t.Fatal(err)
	}
	scale, err := rd.Next()
	if err != nil || scale.ID != IDTimestampScale {
		t.Fatalf("scale header: %v", err)
	}
	payload, err := rd.ReadAll(scale)
	if err != nil || !bytes.Equal(payload, []byte{0x40, 0x00}) {
		t.Fatalf("scale payload=% x err=%v", payload, err)
	}
	if _, err := rd.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("info frame should close: %v", err)
	}
	if err := rd.Leave(); err != nil {
		t.Fatal(err)
	}
	cl, err := rd.Next()
	if err != nil || cl.ID != IDCluster {
		t.Fatalf("cluster header: %v", err)
	}
	if err := rd.Leave(); err != nil {
		t.Fatal(err)
	}
	if _, err := rd.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("document should close: %v", err)
	}
}

func TestUnknownSizeBoundedByParent(t *testing.T) {
	// Per RFC 9559 an unknown-data-size element is the last child of its
	// parent and ends at the parent boundary. Build a sized cluster whose
	// final child is an unknown-size block group.
	inner := append(elem(IDTimecode, []byte{0x81}),
		elemUnknown(IDBlockGroup, elem(IDBlock, []byte{1, 2, 3}))...)
	doc := elem(IDCluster, inner)
	rd := NewReader(bytes.NewReader(doc))

	cl, err := rd.Next()
	if err != nil {
		t.Fatal(err)
	}
	if err := rd.Enter(cl); err != nil {
		t.Fatal(err)
	}
	tc, err := rd.Next()
	if err != nil || tc.ID != IDTimecode {
		t.Fatalf("timecode header: %v", err)
	}
	if err := rd.Skip(tc); err != nil {
		t.Fatal(err)
	}
	bg, err := rd.Next()
	if err != nil || bg.ID != IDBlockGroup || bg.Size != -1 {
		t.Fatalf("block group header: %+v err=%v", bg, err)
	}
	if err := rd.Enter(bg); err != nil {
		t.Fatal(err)
	}
	blk, err := rd.Next()
	if err != nil || blk.ID != IDBlock {
		t.Fatalf("block header: %v", err)
	}
	if _, err := rd.ReadAll(blk); err != nil {
		t.Fatal(err)
	}
	// Unknown frame ends exactly at the sized cluster boundary.
	if _, err := rd.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("unknown frame must close on parent boundary, got %v", err)
	}
	if err := rd.Leave(); err != nil { // pop block group
		t.Fatal(err)
	}
	if _, err := rd.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("cluster must end, got %v", err)
	}
	if err := rd.Leave(); err != nil { // pop cluster
		t.Fatal(err)
	}
	if _, err := rd.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("document must end at EOF, got %v", err)
	}
}

func TestUnknownSizeSegmentClosesAtEOF(t *testing.T) {
	// An unknown-size segment is the common real-world case and runs to
	// EOF; its sized children still parse with normal boundaries.
	doc := elemUnknown(IDSegment, elem(IDCluster, elem(IDTimecode, []byte{0x80})))
	rd := NewReader(bytes.NewReader(doc))
	seg, err := rd.Next()
	if err != nil || seg.Size != -1 {
		t.Fatalf("segment: %+v err=%v", seg, err)
	}
	if err := rd.Enter(seg); err != nil {
		t.Fatal(err)
	}
	cl, err := rd.Next()
	if err != nil || cl.ID != IDCluster {
		t.Fatalf("cluster: %v", err)
	}
	if err := rd.Enter(cl); err != nil {
		t.Fatal(err)
	}
	tc, err := rd.Next()
	if err != nil || tc.ID != IDTimecode {
		t.Fatalf("timecode: %v", err)
	}
	if err := rd.Skip(tc); err != nil {
		t.Fatal(err)
	}
	if _, err := rd.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("sized cluster must end, got %v", err)
	}
	if err := rd.Leave(); err != nil {
		t.Fatal(err)
	}
	if _, err := rd.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("unknown segment must close cleanly at EOF, got %v", err)
	}
}

func TestTruncationReportsFirstMissingOffset(t *testing.T) {
	// Two clusters inside one sized segment; cut the final byte so the
	// segment frame is breached and the first missing offset is exact.
	body := append(elem(IDCluster, elem(IDTimecode, []byte{0x80})),
		elem(IDCluster, elem(IDTimecode, []byte{0x80, 0x00}))...)
	full := elem(IDSegment, body)
	cut := full[:len(full)-1]
	rd := NewReader(bytes.NewReader(cut))

	seg, err := rd.Next()
	if err != nil {
		t.Fatal(err)
	}
	if err := rd.Enter(seg); err != nil {
		t.Fatal(err)
	}
	first, err := rd.Next()
	if err != nil || first.ID != IDCluster {
		t.Fatalf("first cluster: %+v err=%v", first, err)
	}
	// Drain the stream until the missing byte is reported.
	var te *TruncatedError
	for {
		h, err := rd.Next()
		if errors.As(err, &te) {
			break
		}
		if err != nil {
			t.Fatalf("expected TruncatedError, got %v", err)
		}
		if IsMaster(h.ID) {
			if err := rd.Enter(h); err != nil {
				t.Fatal(err)
			}
			if err := rd.Leave(); err != nil {
				if errors.As(err, &te) {
					break
				}
				t.Fatal(err)
			}
		} else if err := rd.Skip(h); err != nil {
			if errors.As(err, &te) {
				break
			}
			t.Fatal(err)
		}
	}
	if te.MissingOffset != int64(len(cut)) {
		t.Fatalf("missing offset = %d, want %d", te.MissingOffset, len(cut))
	}
	// Reader must refuse to continue past the marked offset.
	if _, err := rd.Next(); err == nil {
		t.Fatal("reader continued after truncation")
	}
}

func TestOutOfRangeBlockRejected(t *testing.T) {
	// Cluster declares 6 payload bytes; block inside claims 10.
	block := elem(IDSimpleBlock, make([]byte, 10))
	cluster := append(encID(IDCluster), encVInt(6, 0)...)
	cluster = append(cluster, block...)
	rd := NewReader(bytes.NewReader(cluster))
	cl, err := rd.Next()
	if err != nil {
		t.Fatal(err)
	}
	if err := rd.Enter(cl); err != nil {
		t.Fatal(err)
	}
	if _, err := rd.Next(); err == nil {
		t.Fatal("expected boundary error for oversized child")
	} else {
		var be *BoundaryError
		if !errors.As(err, &be) {
			t.Fatalf("expected BoundaryError, got %T %v", err, err)
		}
	}
}

func TestElementSizeLimit(t *testing.T) {
	// 8-byte size VINT (1 lead + 7 rest) encoding an enormous payload.
	big := append(encID(IDBlock), 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFE)
	rd := NewReader(bytes.NewReader(big))
	_, err := rd.Next()
	var le *LimitError
	if !errors.As(err, &le) {
		t.Fatalf("expected LimitError, got %v", err)
	}
}

func TestLeaveSkipsUnreadBytes(t *testing.T) {
	doc := elem(IDTracks,
		append(elem(IDTrackEntry, elem(IDTrackNumber, []byte{0x83})),
			elem(IDTrackEntry, elem(IDTrackNumber, []byte{0x84}))...))
	rd := NewReader(bytes.NewReader(doc))
	root, err := rd.Next()
	if err != nil {
		t.Fatal(err)
	}
	if err := rd.Enter(root); err != nil {
		t.Fatal(err)
	}
	first, err := rd.Next()
	if err != nil {
		t.Fatal(err)
	}
	if err := rd.Enter(first); err != nil {
		t.Fatal(err)
	}
	if err := rd.Leave(); err != nil { // skip unread TrackNumber
		t.Fatal(err)
	}
	second, err := rd.Next()
	if err != nil || second.ID != IDTrackEntry {
		t.Fatalf("resync after Leave failed: %v", err)
	}
}
