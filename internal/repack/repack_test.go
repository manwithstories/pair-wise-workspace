package repack

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"tsbatch/internal/fault"
	"tsbatch/internal/inspect"
	"tsbatch/internal/ts"
)

func mediaPacket(pid uint16, pusi bool, cc uint8, payload []byte) [ts.PacketSize]byte {
	var p [ts.PacketSize]byte
	p[0] = ts.SyncByte
	p[1] = byte(pid>>8) & 0x1F
	if pusi {
		p[1] |= 0x40
	}
	p[2] = byte(pid & 0xFF)
	p[3] = 0x10 | cc
	copy(p[4:], payload)
	return p
}

func TestSegmentHashAndVerify(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf, PolicyIsolate)
	media := map[uint16]bool{0x101: true}
	mk := func(payload ...byte) inspect.PacketEvent {
		raw := mediaPacket(0x101, false, 0, payload)
		h, _ := ts.ParseHeader(&raw)
		return inspect.PacketEvent{Header: h, Raw: &raw, Media: true}
	}
	w.BeginSegment(0, 0, 0)
	w.Append(mk(1, 2, 3))
	w.Append(mk(4, 5, 6))
	w.EndSegment(0, 1, 9000)
	w.BeginSegment(1, 2, 9000)
	w.Append(mk(7, 8, 9))
	w.EndSegment(1, 2, 18000)
	w.Isolate(10, 12, fault.CatCCGap)

	if len(w.Segments) != 2 || len(w.Isolations) != 1 {
		t.Fatalf("segments=%d isolations=%d", len(w.Segments), len(w.Isolations))
	}
	if w.Isolations[0].Dropped {
		t.Fatal("isolate policy must not mark dropped")
	}
	sizes := []int{w.Segments[0].Packets, w.Segments[1].Packets}
	hashes, _, err := Verify(bytes.NewReader(buf.Bytes()), sizes, media)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 2 || hashes[0] != w.Segments[0].SHA256 || hashes[1] != w.Segments[1].SHA256 {
		t.Fatalf("hash mismatch: %v vs %v", hashes, w.Segments)
	}
	if len(w.Segments[0].SHA256) != 64 {
		t.Fatalf("hash must be 64 hex chars: %q", w.Segments[0].SHA256)
	}
}

func TestDropPolicyAndDeterminism(t *testing.T) {
	build := func() string {
		var buf bytes.Buffer
		w := New(&buf, PolicyDrop)
		w.BeginSegment(0, 0, 0)
		raw := mediaPacket(0x101, false, 0, []byte{9, 9, 9})
		h, _ := ts.ParseHeader(&raw)
		w.Append(inspect.PacketEvent{Header: h, Raw: &raw, Media: true})
		w.EndSegment(0, 0, 0)
		w.Isolate(1, 3, fault.CatTEI)
		if !w.Isolations[0].Dropped {
			t.Fatal("drop policy must mark dropped")
		}
		return w.Segments[0].SHA256
	}
	if build() != build() {
		t.Fatal("same input must produce the same hash")
	}
}

func TestManifestRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "archive.mp4")
	segs := []SegmentInfo{
		{ID: 0, FirstPacket: 0, LastPacket: 99, SHA256: "aaaa"},
		{ID: 1, FirstPacket: 100, LastPacket: 199, SHA256: "bbbb"},
	}
	if err := WriteManifest(path, segs); err != nil {
		t.Fatal(err)
	}
	hashes, err := ReadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 2 || hashes[0] != "aaaa" || hashes[1] != "bbbb" {
		t.Fatalf("manifest hashes=%v", hashes)
	}
}

func TestCreateRefusesInputOverwriteAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ts")
	if err := os.WriteFile(in, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(in, in, PolicyIsolate); err == nil {
		t.Fatal("must refuse to overwrite input")
	}
	fw, err := Create(filepath.Join(dir, "out.ts"), in, PolicyIsolate)
	if err != nil {
		t.Fatal(err)
	}
	tmp := fw.tmp.Name()
	fw.Abort()
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("abort must remove the temp file")
	}
}
