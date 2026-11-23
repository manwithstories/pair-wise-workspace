package parse

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"archivecheck/internal/fault"
)

func box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	n := uint32(len(b))
	b[0], b[1], b[2], b[3] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	copy(b[4:], typ)
	copy(b[8:], payload)
	return b
}

func writeTemp(t *testing.T, data []byte) *os.File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "seg.m4s")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestWalkFileOffsets(t *testing.T) {
	data := append(box("styp", []byte("xxxx")), box("free", nil)...)
	f := writeTemp(t, data)
	var got []Header
	if err := WalkFile(f, "seg", func(h Header, r io.ReadSeeker) error {
		got = append(got, h)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Type != "styp" || got[0].Offset != 0 || got[0].Size != 12 {
		t.Fatalf("headers = %+v", got)
	}
	if got[1].Offset != 12 || got[1].Size != 8 {
		t.Fatalf("second header = %+v", got[1])
	}
}

func TestBoundsFaults(t *testing.T) {
	cases := []struct {
		name   string
		data   []byte
		kind   fault.Kind
		offset int64
	}{
		{"truncated-header", []byte{0, 0, 0, 8, 'm', 'o', 'o'}, fault.KindShortRead, 0},
		{"truncated-payload", []byte{0, 0, 0, 32, 'm', 'o', 'o', 'f'}, fault.KindOutOfBounds, 0},
		{"size-below-header", []byte{0, 0, 0, 4, 'm', 'o', 'o', 'f'}, fault.KindInvalidSize, 0},
		{"extended-truncated", []byte{0, 0, 0, 1, 'm', 'o', 'o', 'f', 0, 0, 0, 0}, fault.KindShortRead, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := writeTemp(t, tc.data)
			err := WalkFile(f, "bad.m4s", func(h Header, r io.ReadSeeker) error { return nil })
			fe, ok := fault.As(err)
			if !ok || fe.Kind != tc.kind || fe.Offset != tc.offset {
				t.Fatalf("err = %v, want kind=%s offset=%d", err, tc.kind, tc.offset)
			}
			if fe.Segment != "bad.m4s" {
				t.Fatalf("segment = %q", fe.Segment)
			}
		})
	}
}

func TestTruncatedAt4096(t *testing.T) {
	free := make([]byte, 4096)
	copy(free, box("free", make([]byte, 4088)))
	moof := make([]byte, 8)
	moof[3] = 0x10 // size 4096
	copy(moof[4:], "moof")
	data := append(free, moof...)
	f := writeTemp(t, data)
	err := WalkFile(f, "truncated.m4s", func(h Header, r io.ReadSeeker) error { return nil })
	fe, ok := fault.As(err)
	if !ok || fe.Offset != 4096 || fe.BoxType != "moof" {
		t.Fatalf("err = %v", err)
	}
}

func TestWalkPayload(t *testing.T) {
	data := append(box("mfhd", bytes.Repeat([]byte{0}, 8)), box("tfdt", bytes.Repeat([]byte{1}, 8))...)
	var types []string
	if err := WalkPayload(data, 100, "seg", func(h Header, p []byte) error {
		types = append(types, h.Type)
		if h.Offset != 100 && h.Offset != 116 {
			t.Fatalf("offset = %d", h.Offset)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(types, ",") != "mfhd,tfdt" {
		t.Fatalf("types = %v", types)
	}
}
