package fingerprint

import "testing"

func TestOffsetDuplicateMatches(t *testing.T) {
	// Timestamps are absent from Window; IDs/track differ but samples match.
	a := Window{ID: "director@10s", TrackID: 1, Samples: [][]byte{[]byte("keyframe"), []byte("p1"), []byte("p2")}}
	b := Window{ID: "stage@25s", TrackID: 2, Samples: [][]byte{[]byte("keyframe"), []byte("p1"), []byte("p2")}}
	c := Window{ID: "different", TrackID: 3, Samples: [][]byte{[]byte("keyframe"), []byte("p1-x"), []byte("p2")}}

	pairs, fps := Dedupe([]Window{a, b, c})
	if len(pairs) != 1 {
		t.Fatalf("pairs=%#v fingerprints=%#v", pairs, fps)
	}
	if pairs[0].KeptID != "director@10s" || pairs[0].DuplicateID != "stage@25s" {
		t.Fatalf("bad pair: %#v", pairs[0])
	}
}

func TestCheapHashCollisionRequiresSecondCompare(t *testing.T) {
	a := Of(Window{ID: "a", Samples: [][]byte{[]byte("same-hash?")}})
	b := Of(Window{ID: "b", Samples: [][]byte{[]byte("different-sample")}})

	// Simulate an adversarial cheap-hash collision.
	b.CheapHash = a.CheapHash
	if SameContent(a, b) {
		t.Fatal("sample summary comparison must reject hash collision")
	}

	b.SampleDigests = append([]string(nil), a.SampleDigests...)
	b.Digest = a.Digest
	b.SampleCount = a.SampleCount
	b.TotalBytes = a.TotalBytes
	if !SameContent(a, b) {
		t.Fatal("identical sample summaries should verify")
	}
}
