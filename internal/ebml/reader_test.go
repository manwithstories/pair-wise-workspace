package ebml

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"mediaproc/internal/model"
)

func bytesReader(b []byte) io.ReadSeeker { return bytes.NewReader(b) }

func TestVINTAndElementHeader(t *testing.T) {
	payload := []byte{1, 2, 3}
	raw := BuildElement(IDTimecode, payload)
	r, err := NewReader(bytesReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	el, err := r.ReadElementHeader()
	if err != nil {
		t.Fatal(err)
	}
	if el.ID != IDTimecode {
		t.Fatalf("id=%x want %x", el.ID, IDTimecode)
	}
	if el.Offset != 0 || el.Size != 3 || el.Unknown {
		t.Fatalf("element=%+v", el)
	}
	got, err := r.ReadPayload(el, 16)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload=%v", got)
	}
}

func TestUnknownLengthHeader(t *testing.T) {
	raw := UnknownHeader(IDCluster, 8)
	if len(raw) != 12 { // four-byte ID + eight-byte VINT
		t.Fatalf("header length=%d", len(raw))
	}
	r, _ := NewReader(bytesReader(raw))
	el, err := r.ReadElementHeader()
	if err != nil {
		t.Fatal(err)
	}
	if el.ID != IDCluster || !el.Unknown || el.Size != -1 {
		t.Fatalf("unexpected element: %+v", el)
	}
}

func TestEncodeSizeKnownAndUnknownBoundaries(t *testing.T) {
	cases := []uint64{0, 1, 126, 127, 16382, 16383}
	for _, n := range cases {
		b := EncodeSize(n)
		r, _ := NewReader(bytesReader(b))
		v, err := r.ReadVINT(false)
		if err != nil {
			t.Fatalf("size %d: %v", n, err)
		}
		if v.Unknown || v.Value != n {
			t.Fatalf("size %d decoded=%+v", n, v)
		}
	}
	b := UnknownSize(2)
	r, _ := NewReader(bytesReader(b))
	v, err := r.ReadVINT(false)
	if err != nil || !v.Unknown || v.Value != 0x3fff {
		t.Fatalf("unknown size decode v=%+v err=%v", v, err)
	}
}

func TestIllegalVINTOffset(t *testing.T) {
	// A valid Cluster ID followed by an all-zero size descriptor.
	raw := append(EncodeID(IDCluster), 0x00, 0x01)
	r, _ := NewReader(bytesReader(raw))
	_, err := r.ReadElementHeader()
	var fe *model.Error
	if !errors.As(err, &fe) {
		t.Fatalf("expected model.Error, got %T %v", err, err)
	}
	if fe.Offset != 4 || fe.Phase != model.PhaseEBML || !fe.Recoverable {
		t.Fatalf("fault=%+v", fe)
	}
}

func TestTruncatedPayloadRetainsElementOffset(t *testing.T) {
	raw := ElementHeader(IDSimpleBlock, 64)
	raw = append(raw, make([]byte, 10)...)
	r, _ := NewReader(bytesReader(raw))
	el, err := r.ReadElementHeader()
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.ReadPayload(el, 128)
	var fe *model.Error
	if !errors.As(err, &fe) || fe.Offset != 0 {
		t.Fatalf("want offset 0 fault, got %v", err)
	}
}

func TestPayloadLimitIsBounded(t *testing.T) {
	raw := ElementHeader(IDBlock, 1024)
	r, _ := NewReader(bytesReader(raw))
	el, err := r.ReadElementHeader()
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.ReadPayload(el, 16)
	var fe *model.Error
	if !errors.As(err, &fe) || fe.Offset != 0 {
		t.Fatalf("expected bounded fault at element offset, got %v", err)
	}
}

func TestFindNextElementIsBoundedAndFindsBoundary(t *testing.T) {
	good := UintElement(IDTimecode, 7)
	noise := []byte{0x00, 0x01, 0x02, 0x1F}
	raw := append(noise, good...)
	r, _ := NewReader(bytesReader(raw))
	el, err := r.FindNextElement(IDTimecode, 16)
	if err != nil {
		t.Fatal(err)
	}
	if el.Offset != int64(len(noise)) || el.ID != IDTimecode {
		t.Fatalf("element=%+v want offset %d", el, len(noise))
	}

	r, _ = NewReader(bytesReader([]byte{0, 0, 0}))
	_, err = r.FindNextElement(IDCluster, 2)
	if err == nil {
		t.Fatal("expected bounded resync failure")
	}
}
