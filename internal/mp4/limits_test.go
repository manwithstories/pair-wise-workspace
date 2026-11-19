package mp4_test

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"archivearchivist/internal/mp4"
)

// TestDeclaredSampleCountIsBounded guards the memory budget: an stsz that
// claims 400 million constant-size samples must be refused with a located
// fault instead of allocating gigabytes of index state.
func TestDeclaredSampleCountIsBounded(t *testing.T) {
	u32 := func(v ...uint32) []byte {
		out := make([]byte, 4*len(v))
		for i, x := range v {
			binary.BigEndian.PutUint32(out[i*4:], x)
		}
		return out
	}
	mdhdPayload := append(u32(0, 0, 0, 90000, 0), 0x55, 0xc4, 0, 0)
	tkhdPayload := append(u32(0, 0, 0, 1, 0), make([]byte, 60)...)

	stbl := cat(
		mkBox("stts", u32(0, 1, 10, 3000)),
		mkBox("stsz", u32(0, 300, 400_000_000)), // constant size, absurd count
		mkBox("stco", u32(0, 1, 0)),
		mkBox("stsc", u32(0, 1, 1, 10, 1)),
	)
	trak := cat(
		mkBox("tkhd", tkhdPayload),
		mkBox("mdia", cat(
			mkBox("mdhd", mdhdPayload),
			mkBox("minf", mkBox("stbl", stbl)),
		)),
	)
	image := cat(
		mkBox("ftyp", append([]byte("iso6"), u32(0)...)),
		mkBox("moov", mkBox("trak", trak)),
	)

	sc, err := mp4.ScanReader(bytes.NewReader(image), int64(len(image)), "hostile.mp4", "V1")
	if err != nil {
		t.Fatalf("scan returned an error instead of a fault: %v", err)
	}
	if len(sc.Tracks) != 0 {
		t.Fatalf("hostile sample table produced %d tracks", len(sc.Tracks))
	}
	found := false
	for _, f := range sc.Faults {
		if strings.Contains(f.Detail, "sample limit") {
			found = true
		}
	}
	if !found {
		t.Fatalf("sample-count limit not reported: %v", sc.Faults)
	}
}

// mkBox frames payload as a plain 32-bit box.
func mkBox(typ string, payload []byte) []byte {
	out := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(out[:4], uint32(len(payload)+8))
	copy(out[4:], typ)
	return append(out, payload...)
}

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
