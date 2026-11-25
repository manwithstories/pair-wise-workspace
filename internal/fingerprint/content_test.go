package fingerprint

import (
	"encoding/hex"
	"testing"
)

func TestHashesAre64HexChars(t *testing.T) {
	clip := ClipSHA256([]byte("clip bytes"))
	if len(clip) != 64 {
		t.Fatalf("clip hash length=%d", len(clip))
	}
	if _, err := hex.DecodeString(clip); err != nil {
		t.Fatal(err)
	}
}

func TestContentFingerprintIgnoresTimestampOffsetAndHeaders(t *testing.T) {
	payload := []byte{0, 1, 2, 3, 7, 9}
	f1 := Content([]Chunk{{PTSTicks: 100, Payload: payload}})
	f2 := Content([]Chunk{{PTSTicks: 9_000_100, Payload: append([]byte(nil), payload...)}})
	if f1 != f2 {
		t.Fatalf("fingerprint changed with PTS offset: %s vs %s", f1, f2)
	}
	other := ContentBytes(append([]byte(nil), payload...))
	if f1 != other {
		t.Fatal("single chunk helper mismatch")
	}
	changed := ContentBytes(append(payload, 0xff))
	if f1 == changed {
		t.Fatal("payload change did not change fingerprint")
	}
}

func TestDedupRecordsSourcesOnce(t *testing.T) {
	d := NewDedupIndex()
	s1 := Source{ClipID: "clip-2", Source: "studio-a"}
	s2 := Source{ClipID: "clip-5", Source: "remote-cam"}
	if first, _ := d.Observe("fp", s1, "blobs/fp"); !first {
		t.Fatal("first observation should write")
	}
	if first, _ := d.Observe("fp", s2, ""); first {
		t.Fatal("second observation must not write another blob")
	}
	if first, _ := d.Observe("fp", s1, ""); first {
		t.Fatal("identical source should be idempotent")
	}
	r, ok := d.Record("fp")
	if !ok || len(r.Sources) != 2 || r.BlobPath != "blobs/fp" {
		t.Fatalf("bad record: %+v ok=%v", r, ok)
	}
}
