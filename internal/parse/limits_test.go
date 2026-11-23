package parse

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"archivecheck/internal/fault"
)

func TestLargeBoxPayloadRejected(t *testing.T) {
	// Declare a 16 MiB box (over MaxBoxPayload) backed by a sparse file so the
	// header bounds check passes and only the payload cap rejects it.
	size := uint64(MaxBoxPayload) * 2
	p := filepath.Join(t.TempDir(), "big.mp4")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	hdr := make([]byte, 16)
	hdr[3] = 1
	copy(hdr[4:], "moov")
	for i := 0; i < 8; i++ {
		hdr[8+i] = byte(size >> (56 - 8*uint(i)))
	}
	if _, err := f.Write(hdr); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(size)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	g, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	var payloadErr error
	if err := WalkFile(g, "big.mp4", func(h Header, r io.ReadSeeker) error {
		if h.Type == "moov" {
			_, payloadErr = ReadPayload(r, h, "big.mp4")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !fault.IsKind(payloadErr, fault.KindInvalidSize) {
		t.Fatalf("err = %v, want invalid size fault", payloadErr)
	}
}

func TestChildBoxOverflowInsidePayload(t *testing.T) {
	child := []byte{0x7f, 0xff, 0xff, 0xff, 't', 'r', 'u', 'n'}
	err := WalkPayload(child, 100, "seg", func(h Header, p []byte) error { return nil })
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindOutOfBounds || fe.Offset != 100 {
		t.Fatalf("err = %v", err)
	}
}

func TestHugeExtendedSizeDoesNotOverflow(t *testing.T) {
	// A 64-bit largesize near MaxUint64 must be rejected as out of bounds
	// without integer overflow or allocation.
	data := make([]byte, 16)
	data[3] = 1
	copy(data[4:], "mdat")
	for i := 0; i < 8; i++ {
		data[8+i] = 0xff
	}
	f := writeTemp(t, data)
	walkErr := WalkFile(f, "huge.m4s", func(h Header, r io.ReadSeeker) error { return nil })
	if walkErr == nil {
		t.Fatal("huge extended size accepted")
	}
	fe, ok := fault.As(walkErr)
	if !ok || fe.Offset != 0 {
		t.Fatalf("fault = %v", walkErr)
	}
	if fe.Kind != fault.KindOutOfBounds {
		t.Fatalf("kind = %s, want out_of_bounds", fe.Kind)
	}
}
