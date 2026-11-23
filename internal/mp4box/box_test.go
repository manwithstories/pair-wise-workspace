package mp4box

import (
	"bytes"
	"testing"

	"mediacore/internal/fault"
	"mediacore/internal/fixture"
)

func TestChildrenAndFind(t *testing.T) {
	art, err := fixture.Build(fixture.DefaultSpec(42, 0))
	if err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(art.Bytes)
	root := &Header{PayloadOff: 0, End: int64(len(art.Bytes))}
	tops, err := Children(r, root, int64(len(art.Bytes)))
	if err != nil {
		t.Fatal(err)
	}
	if Find(tops, boxType("ftyp")) == nil || Find(tops, boxType("moov")) == nil ||
		Find(tops, boxType("mdat")) == nil {
		t.Fatalf("missing top-level boxes: %+v", tops)
	}
}

func TestTailFragmentTruncatedHeader(t *testing.T) {
	spec := fixture.DefaultSpec(42, 0)
	spec.Mode = fixture.ModeTruncatedHeader
	art, _ := fixture.Build(spec)
	r := bytes.NewReader(art.Bytes)
	tops, err := Children(r, &Header{PayloadOff: 0, End: int64(len(art.Bytes))},
		int64(len(art.Bytes)))
	if err == nil {
		f := TailFragment(bytes.NewReader(art.Bytes), tops, int64(len(art.Bytes)))
		// Either the scanner aborts directly or TailFragment catches the half head.
		if f == nil {
			t.Fatal("structural half-header truncation must be detected")
		}
		if f.Kind != fault.KindTruncatedHeader || f.Recoverable() {
			t.Fatalf("want aborting truncated-header fault, got %v", f)
		}
	}
}

func TestTruncatedMdatTolerated(t *testing.T) {
	spec := fixture.DefaultSpec(42, 0)
	spec.Mode = fixture.ModeTruncatedTail
	art, _ := fixture.Build(spec)
	tops, err := Children(bytes.NewReader(art.Bytes),
		&Header{PayloadOff: 0, End: int64(len(art.Bytes))}, int64(len(art.Bytes)))
	if err != nil {
		t.Fatalf("truncated mdat tail must not abort box scan: %v", err)
	}
	mdat := Find(tops, boxType("mdat"))
	if mdat == nil || mdat.End != -1 {
		t.Fatalf("mdat must be present and marked EOF-extended: %+v", mdat)
	}
}

func TestPayloadLimit(t *testing.T) {
	art, _ := fixture.Build(fixture.DefaultSpec(42, 0))
	tops, _ := Children(bytes.NewReader(art.Bytes),
		&Header{PayloadOff: 0, End: int64(len(art.Bytes))}, int64(len(art.Bytes)))
	ftyp := Find(tops, boxType("ftyp"))
	if _, err := Payload(bytes.NewReader(art.Bytes), ftyp, 4); err == nil {
		t.Fatal("oversized payload read must be rejected")
	}
}

func boxType(s string) (t [4]byte) { copy(t[:], s); return }
