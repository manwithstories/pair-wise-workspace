package dedup

import (
	"testing"

	"mediad/internal/codec"
)

func TestFingerprintIgnoresTimestampsAndLocation(t *testing.T) {
	c1 := Content{Codec: H264Test, Config: []byte("sps-pps"), AU: []byte{0, 0, 0, 1, 0x65, 1, 2, 3}}
	c2 := c1
	// Timestamps/locations are not part of Content at all; changing timing
	// context must not alter the hash.
	f1 := Compute(c1)
	f2 := Compute(c2)
	if f1.SHA256 != f2.SHA256 {
		t.Fatal("identical content produced different fingerprints")
	}

	c3 := c1
	c3.AU = append([]byte(nil), c1.AU...)
	c3.AU[len(c3.AU)-1] ^= 0xff
	if Compute(c3).SHA256 == f1.SHA256 {
		t.Fatal("different AU content must change fingerprint")
	}
	c4 := c1
	c4.Config = []byte("other-config")
	if Compute(c4).SHA256 == f1.SHA256 {
		t.Fatal("different codec configuration must change fingerprint")
	}
}

const H264Test = codec.H264

func TestDuplicateIsByteVerifiedAndSavesBytes(t *testing.T) {
	st := NewStore()
	content := Content{Codec: codec.H265, Config: []byte("vps-sps-pps"), AU: make([]byte, 123)}
	fp := Compute(content)
	items := map[string]Content{"node-a/au-7": content}
	load := func(r Ref) (Content, error) { return items[r.Location], nil }

	dup, saved, err := st.CheckAndAdd(fp, content, "node-a/au-7", load)
	if err != nil || dup || saved != 0 {
		t.Fatalf("first add dup=%v saved=%d err=%v", dup, saved, err)
	}
	items["node-b/au-7"] = content
	dup, saved, err = st.CheckAndAdd(fp, content, "node-b/au-7", load)
	if err != nil || !dup || saved != int64(len(content.AU)) {
		t.Fatalf("second add dup=%v saved=%d err=%v", dup, saved, err)
	}
	if st.DuplicateAUs != 1 || st.SavedBytes != int64(len(content.AU)) {
		t.Fatalf("bad stats: %+v", st)
	}
}

func TestHashCollisionIsNotDuplicateOrDeleted(t *testing.T) {
	st := NewStore()
	c1 := Content{Codec: codec.AV1, Config: []byte("seq"), AU: []byte("aaaa")}
	c2 := Content{Codec: codec.AV1, Config: []byte("seq"), AU: []byte("bbbb")} // same length
	fp1 := Compute(c1)
	fp2 := Compute(c2)
	fp2.SHA256 = fp1.SHA256 // deliberately simulate a SHA-256 collision

	items := map[string]Content{"loc-1": c1, "loc-2": c2}
	load := func(r Ref) (Content, error) { return items[r.Location], nil }

	if dup, _, err := st.CheckAndAdd(fp1, c1, "loc-1", load); err != nil || dup {
		t.Fatalf("first item dup=%v err=%v", dup, err)
	}
	dup, saved, err := st.CheckAndAdd(fp2, c2, "loc-2", load)
	if err != nil {
		t.Fatal(err)
	}
	if dup || saved != 0 {
		t.Fatalf("collision must not be treated as duplicate: dup=%v saved=%d", dup, saved)
	}
	if st.HashCollisions != 1 {
		t.Fatalf("collisions=%d want 1", st.HashCollisions)
	}
	if refs := st.Refs(fp1.SHA256); len(refs) != 2 {
		t.Fatalf("colliding items must both remain, refs=%d", len(refs))
	}
}
