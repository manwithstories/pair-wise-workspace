package ebml

import (
	"bytes"
	"io"
	"testing"

	"mediapack/internal/clock"
)

// truncationCase is one input cut point and the exact offset the parser
// must report. The requirement allows <=1 byte deviation.
type truncationCase struct {
	name   string
	cutAt  int64
	wantOff int64
}

func TestTruncationLocalization(t *testing.T) {
	// Layout:
	//  cluster header: 4-byte ID + 1-byte size (=5), body length 9
	//    timecode: 1-byte ID + 1-byte size + 2 body (=4), offsets 5..8
	//    simpleblock: 1-byte ID + 1-byte size + 3 body (=5), offsets 9..13
	blockBody := []byte{0x81, 0x00, 0x20} // track 1, +32ms, flags 0
	block := buildElement(0xA3, blockBody)
	timecode := buildElement(0xE7, []byte{0x00, 0x01})
	clusterBody := append(timecode, block...)
	full := buildElement(0x1F43B675, clusterBody)

	cases := []truncationCase{
		// Cut one byte into the block's payload: error must anchor at
		// the first missing payload byte (offset 13), tolerance 1.
		{"block payload cut", 13, 13},
		// Cut inside the block header: ID is at offset 9, missing size
		// byte at 10 -> reported 10.
		{"block header cut", 10, 10},
		// Cut inside timecode body (physical cut at 7): the declared
		// element end (first missing logical byte) is offset 9.
		{"timecode body cut", 7, 9},
		// Cut inside cluster size field (offset 4 missing).
		{"cluster header cut", 4, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cut := append([]byte(nil), full[:tc.cutAt]...)
			r := NewReader(bytes.NewReader(cut), 1<<20)
			err := walkAll(r)
			me, ok := clock.AsMediaError(err)
			if !ok {
				t.Fatalf("want MediaError, got %v", err)
			}
			if me.Category != clock.CatTruncated {
				t.Fatalf("want CatTruncated, got %s", me.Category)
			}
			diff := me.Offset - tc.wantOff
			if diff < 0 {
				diff = -diff
			}
			if diff > 1 {
				t.Fatalf("offset error %d bytes: got %d want %d", diff, me.Offset, tc.wantOff)
			}
		})
	}
}

// walkAll consumes every element, descending into known containers.
func walkAll(r *Reader) error {
	for {
		el, err := r.Next()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if isContainer(el.ID) {
			r.Push(el)
			if err := walkAll(r); err != nil {
				return err
			}
			if err := r.Leave(); err != nil {
				return err
			}
		} else if err := r.DiscardBody(el); err != nil {
			return err
		}
	}
}

func isContainer(id uint64) bool {
	switch id {
	case IDSegment, IDCluster, IDTracks, IDTrackEntry, IDBlockGroup, IDInfo:
		return true
	}
	return false
}

func TestUnknownLengthNestedTruncation(t *testing.T) {
	// Unknown-length Segment containing a finite Cluster whose child
	// timecode declares 2 body bytes but only 1 reaches the reader.
	seg := append(EncodeElementID(IDSegment), EncodeUnknownSize(4)...)
	timecode := buildElement(0xE7, []byte{0x00, 0x01}) // 4 bytes total
	clusterBody := timecode[:3]                        // cut one body byte
	cluster := append(EncodeElementID(IDCluster), EncodeVINT(uint64(len(clusterBody))+1)...)
	cluster = append(cluster, clusterBody...)
	cut := append(seg, cluster...)

	r := NewReader(bytes.NewReader(cut), 1<<20)
	segEl, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	r.Push(segEl)
	cl, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	r.Push(cl)
	tc, err := r.Next() // header reads fine
	if err != nil {
		t.Fatal(err)
	}
	err = r.DiscardBody(tc) // body read hits the cut
	me, ok := clock.AsMediaError(err)
	if !ok || me.Category != clock.CatTruncated {
		t.Fatalf("want truncation inside unknown segment, got %v", err)
	}
	// The anchor must be within 1 byte of the physical cut point.
	diff := me.Offset - int64(len(cut))
	if diff < 0 {
		diff = -diff
	}
	if diff > 1 {
		t.Fatalf("nested truncation offset off by %d (got %d, cut %d)", diff, me.Offset, len(cut))
	}
}

func TestDecodeBlock(t *testing.T) {
	// SimpleBlock: track VINT 0x81 (track 1), +300ms, keyframe flag.
	body := []byte{0x81, 0x01, 0x2C, 0x80, 0xAA, 0xBB, 0xCC}
	b, err := DecodeBlock(body, 42, true)
	if err != nil {
		t.Fatal(err)
	}
	if b.Track != 1 || b.RelativeTC != 300 || !b.Keyframe || b.PayloadLen != 3 {
		t.Fatalf("bad block decode: %+v", b)
	}
	if !bytes.Equal(b.Payload, []byte{0xAA, 0xBB, 0xCC}) {
		t.Fatalf("bad payload %x", b.Payload)
	}
	b.SetPayloadOff(50, 0)
	if b.PayloadOff != 50+1+3 {
		t.Fatalf("payload offset=%d", b.PayloadOff)
	}

	// Negative relative timecode and lacing bits.
	body2 := []byte{0x82, 0xFF, 0xFC, 0x02} // track 2, -4 ticks, EBML lacing
	b2, err := DecodeBlock(body2, 7, false)
	if err != nil {
		t.Fatal(err)
	}
	if b2.Track != 2 || b2.RelativeTC != -4 || b2.Lacing() != 1 || b2.Keyframe {
		t.Fatalf("bad block2: %+v", b2)
	}

	// Too-short body must be a CatBadBlock anchored at the header.
	if _, err := DecodeBlock([]byte{0x81, 0x00}, 99, true); err == nil {
		t.Fatal("expected bad block error")
	} else if me, ok := clock.AsMediaError(err); !ok || me.Category != clock.CatBadBlock || me.Offset != 99 {
		t.Fatalf("want anchored CatBadBlock, got %v", err)
	}
}
